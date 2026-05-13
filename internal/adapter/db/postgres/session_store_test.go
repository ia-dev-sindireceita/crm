package postgres_test

// SIN-62213 integration tests for SessionStore + UserCredentialReader
// against a real Postgres. Uses the testpg harness from PR4 (SIN-62212).
//
// The existing harness only applies migrations 0002+0003 against each
// per-test DB; this file extends each test DB with 0004-0006 (tenants,
// users, sessions) so the IAM adapters have somewhere to land. This is
// strictly additive — existing withtenant_test.go semantics are
// unchanged.

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/iam"
)

// freshDBWithIAM builds a per-test DB and applies 0004-0006 on top of the
// harness's default 0001-0003 sequence. Cleanup is handled by the
// underlying testpg.DB t.Cleanup hook.
func freshDBWithIAM(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0006_create_sessions.up.sql",
	} {
		path := filepath.Join(harness.MigrationsDir(), name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := db.AdminPool().Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	return db
}

// seedTenant inserts a tenant + user (with a known password hash) using
// the admin pool (BYPASSRLS) so the test can later exercise the runtime
// pool's RLS-bound view. Returns the tenant id, user id, and the
// plaintext password (so tests can hand it to Login).
//
// email is taken explicitly so multi-tenant tests don't accidentally seed
// the same email into both tenants and mask RLS leaks (the cross-tenant
// test relies on this).
func seedTenant(t *testing.T, db *testpg.DB, host, email string) (tenantID, userID uuid.UUID, plaintext string) {
	t.Helper()
	tenantID = uuid.New()
	userID = uuid.New()
	plaintext = "correct-horse-battery-staple"
	hash, err := iam.HashPassword(plaintext)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantID, host, host); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, $4, 'agent')`,
		userID, tenantID, email, hash); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return tenantID, userID, plaintext
}

func TestSessionStore_CreateGetDelete(t *testing.T) {
	db := freshDBWithIAM(t)
	tenantID, userID, _ := seedTenant(t, db, "acme.crm.local", "alice@acme.test")

	store := postgres.NewSessionStore(db.RuntimePool())
	ctx := context.Background()

	id, err := iam.NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	sess := iam.Session{
		ID:        id,
		TenantID:  tenantID,
		UserID:    userID,
		ExpiresAt: now.Add(time.Hour),
		CreatedAt: now,
		IPAddr:    net.IPv4(192, 0, 2, 7).To4(),
		UserAgent: "ua/test",
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Get(ctx, tenantID, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != sess.ID || got.UserID != sess.UserID || got.TenantID != sess.TenantID {
		t.Fatalf("ids round-trip wrong: %#v", got)
	}
	if !got.ExpiresAt.Equal(sess.ExpiresAt) {
		t.Fatalf("ExpiresAt mismatch: got %v want %v", got.ExpiresAt, sess.ExpiresAt)
	}
	if got.UserAgent != "ua/test" {
		t.Fatalf("UserAgent mismatch: got %q", got.UserAgent)
	}
	if !got.IPAddr.Equal(net.IPv4(192, 0, 2, 7)) {
		t.Fatalf("IPAddr mismatch: got %v", got.IPAddr)
	}

	if err := store.Delete(ctx, tenantID, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, tenantID, id); !errors.Is(err, iam.ErrSessionNotFound) {
		t.Fatalf("after Delete, Get err=%v want ErrSessionNotFound", err)
	}
}

func TestSessionStore_DeleteIdempotent(t *testing.T) {
	db := freshDBWithIAM(t)
	tenantID, _, _ := seedTenant(t, db, "acme.crm.local", "alice@acme.test")
	store := postgres.NewSessionStore(db.RuntimePool())
	if err := store.Delete(context.Background(), tenantID, uuid.New()); err != nil {
		t.Fatalf("Delete of unknown id should be idempotent: %v", err)
	}
}

func TestSessionStore_GetUnknown_ReturnsNotFound(t *testing.T) {
	db := freshDBWithIAM(t)
	tenantID, _, _ := seedTenant(t, db, "acme.crm.local", "alice@acme.test")
	store := postgres.NewSessionStore(db.RuntimePool())
	if _, err := store.Get(context.Background(), tenantID, uuid.New()); !errors.Is(err, iam.ErrSessionNotFound) {
		t.Fatalf("err=%v want ErrSessionNotFound", err)
	}
}

// TestSessionStore_CrossTenantProbe_CollapsesToNotFound proves the
// SecurityEngineer review item: a session id that exists in tenant B
// must look identical to "id does not exist" when probed from tenant A.
// Otherwise the error type becomes a side channel for cross-tenant
// session-id enumeration.
func TestSessionStore_CrossTenantProbe_CollapsesToNotFound(t *testing.T) {
	db := freshDBWithIAM(t)
	tenantA, userA, _ := seedTenant(t, db, "acme.crm.local", "alice@acme.test")
	tenantB, userB, _ := seedTenant(t, db, "globex.crm.local", "bob@globex.test")
	store := postgres.NewSessionStore(db.RuntimePool())
	ctx := context.Background()

	// Create a session in tenant B.
	idB, _ := iam.NewSessionID()
	now := time.Now().UTC()
	if err := store.Create(ctx, iam.Session{
		ID: idB, TenantID: tenantB, UserID: userB,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatalf("Create tenantB: %v", err)
	}

	// Probe from tenant A: must be ErrSessionNotFound, identical to a
	// totally-unknown id.
	if _, err := store.Get(ctx, tenantA, idB); !errors.Is(err, iam.ErrSessionNotFound) {
		t.Fatalf("cross-tenant probe err=%v want ErrSessionNotFound", err)
	}

	// Sanity: tenant B can still see its own session.
	if got, err := store.Get(ctx, tenantB, idB); err != nil || got.UserID != userB {
		t.Fatalf("tenantB own session: err=%v userID=%v", err, got.UserID)
	}
	_ = userA
}

func TestSessionStore_DeleteExpired(t *testing.T) {
	db := freshDBWithIAM(t)
	tenantID, userID, _ := seedTenant(t, db, "acme.crm.local", "alice@acme.test")
	store := postgres.NewSessionStore(db.RuntimePool())
	ctx := context.Background()
	now := time.Now().UTC()

	expired1, _ := iam.NewSessionID()
	expired2, _ := iam.NewSessionID()
	live, _ := iam.NewSessionID()
	for _, s := range []iam.Session{
		{ID: expired1, TenantID: tenantID, UserID: userID, ExpiresAt: now.Add(-time.Hour), CreatedAt: now.Add(-2 * time.Hour)},
		{ID: expired2, TenantID: tenantID, UserID: userID, ExpiresAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Hour)},
		{ID: live, TenantID: tenantID, UserID: userID, ExpiresAt: now.Add(time.Hour), CreatedAt: now},
	} {
		if err := store.Create(ctx, s); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	n, err := store.DeleteExpired(ctx, tenantID)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if n != 2 {
		t.Fatalf("DeleteExpired returned %d, want 2", n)
	}
	if _, err := store.Get(ctx, tenantID, live); err != nil {
		t.Fatalf("live session got swept: %v", err)
	}
	if _, err := store.Get(ctx, tenantID, expired1); !errors.Is(err, iam.ErrSessionNotFound) {
		t.Fatalf("expired1 still present: err=%v", err)
	}
}

func TestUserCredentialReader_Hit(t *testing.T) {
	db := freshDBWithIAM(t)
	tenantID, userID, plaintext := seedTenant(t, db, "acme.crm.local", "alice@acme.test")
	reader := postgres.NewUserCredentialReader(db.RuntimePool())
	gotID, gotHash, err := reader.LookupCredentials(context.Background(), tenantID, "alice@acme.test")
	if err != nil {
		t.Fatalf("LookupCredentials: %v", err)
	}
	if gotID != userID {
		t.Fatalf("user id round-trip wrong: got %s want %s", gotID, userID)
	}
	ok, err := iam.VerifyPassword(plaintext, gotHash)
	if err != nil || !ok {
		t.Fatalf("hash from DB does not verify against original plaintext: ok=%v err=%v", ok, err)
	}
}

func TestUserCredentialReader_Miss_ReturnsZeroNoError(t *testing.T) {
	db := freshDBWithIAM(t)
	tenantID, _, _ := seedTenant(t, db, "acme.crm.local", "alice@acme.test")
	reader := postgres.NewUserCredentialReader(db.RuntimePool())
	id, hash, err := reader.LookupCredentials(context.Background(), tenantID, "ghost@acme.test")
	if err != nil {
		t.Fatalf("contract violation: miss must return nil error, got %v", err)
	}
	if id != uuid.Nil || hash != "" {
		t.Fatalf("miss must return zero values, got id=%s hash=%q", id, hash)
	}
}

// TestUserCredentialReader_CrossTenant_HiddenByRLS proves the email of
// tenant A is NOT visible from tenant B via the runtime pool, because RLS
// gates the SELECT on app.tenant_id.
func TestUserCredentialReader_CrossTenant_HiddenByRLS(t *testing.T) {
	db := freshDBWithIAM(t)
	_, _, _ = seedTenant(t, db, "acme.crm.local", "alice@acme.test")
	tenantB, _, _ := seedTenant(t, db, "globex.crm.local", "bob@globex.test")
	reader := postgres.NewUserCredentialReader(db.RuntimePool())
	id, hash, err := reader.LookupCredentials(context.Background(), tenantB, "alice@acme.test")
	if err != nil {
		t.Fatalf("LookupCredentials: %v", err)
	}
	if id != uuid.Nil || hash != "" {
		t.Fatalf("cross-tenant lookup leaked: id=%s hash=%q", id, hash)
	}
}

// TestNewSessionStore_NilPool returns nil so a programming error
// (forgetting to construct the pool) surfaces as a nil-deref at first
// call rather than as a silent "no rows" later.
func TestNewSessionStore_NilPool(t *testing.T) {
	if got := postgres.NewSessionStore(nil); got != nil {
		t.Fatalf("NewSessionStore(nil) = %v, want nil", got)
	}
	if got := postgres.NewUserCredentialReader(nil); got != nil {
		t.Fatalf("NewUserCredentialReader(nil) = %v, want nil", got)
	}
}

func TestSessionStore_Create_RejectsNilTenant(t *testing.T) {
	db := freshDBWithIAM(t)
	store := postgres.NewSessionStore(db.RuntimePool())
	id, _ := iam.NewSessionID()
	err := store.Create(context.Background(), iam.Session{ID: id, TenantID: uuid.Nil})
	if err == nil {
		t.Fatalf("Create with uuid.Nil tenant must fail")
	}
}
