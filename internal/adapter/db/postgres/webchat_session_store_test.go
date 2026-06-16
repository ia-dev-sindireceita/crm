package postgres_test

// SIN-64972 integration tests for the webchat SessionStore + OriginValidator
// against a real Postgres (testpg harness). These run as app_runtime via
// RuntimePool, so they also prove migration 0119's grant fix: webchat_session
// was created by 0096 WITHOUT app_runtime grants, which would have made the
// production store fail with "permission denied" until this migration.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	webchat "github.com/pericles-luz/crm/internal/adapter/channels/webchat"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

// freshDBWithWebchat applies 0004 (tenants) + 0096 (webchat_session) +
// 0119 (origin columns + webchat_session grants) on top of the harness
// default sequence, giving the webchat adapters their schema.
func freshDBWithWebchat(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0096_webchat_session.up.sql",
		"0119_webchat_tenant_origins.up.sql",
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

// seedWebchatTenant inserts a bare tenant row via the admin pool and
// returns its id.
func seedWebchatTenant(t *testing.T, db *testpg.DB, host string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, host, host); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	return id
}

func TestNewWebchatSessionStore_NilPool(t *testing.T) {
	if got := postgres.NewWebchatSessionStore(nil); got != nil {
		t.Fatalf("NewWebchatSessionStore(nil) = %v, want nil", got)
	}
	if got := postgres.NewWebchatOrigins(nil); got != nil {
		t.Fatalf("NewWebchatOrigins(nil) = %v, want nil", got)
	}
}

func TestWebchatSessionStore_CreateGetTouch(t *testing.T) {
	db := freshDBWithWebchat(t)
	tenantID := seedWebchatTenant(t, db, "acme.crm.local")
	store := postgres.NewWebchatSessionStore(db.RuntimePool())
	ctx := context.Background()

	sess := webchat.Session{
		ID:            uuid.Must(uuid.NewV7()).String(),
		TenantID:      tenantID,
		CSRFTokenHash: "csrfhash",
		OriginSig:     "originsig",
		IPHash:        "iphash",
		ExpiresAt:     time.Now().UTC().Add(30 * time.Minute).Truncate(time.Millisecond),
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TenantID != tenantID || got.CSRFTokenHash != "csrfhash" || got.OriginSig != "originsig" || got.IPHash != "iphash" {
		t.Fatalf("Get returned %+v, want fields from %+v", got, sess)
	}

	// Touch slides the idle expiry forward.
	before := got.ExpiresAt
	time.Sleep(10 * time.Millisecond)
	if err := store.Touch(ctx, sess.ID); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	after, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get after touch: %v", err)
	}
	if !after.ExpiresAt.After(before) {
		t.Fatalf("Touch did not extend expiry: before=%v after=%v", before, after.ExpiresAt)
	}
}

func TestWebchatSessionStore_GetUnknown_ReturnsNotFound(t *testing.T) {
	db := freshDBWithWebchat(t)
	store := postgres.NewWebchatSessionStore(db.RuntimePool())
	_, err := store.Get(context.Background(), uuid.Must(uuid.NewV7()).String())
	if !errors.Is(err, webchat.ErrSessionNotFound) {
		t.Fatalf("Get(unknown) err = %v, want ErrSessionNotFound", err)
	}
}

func TestWebchatSessionStore_TouchUnknown_ReturnsNotFound(t *testing.T) {
	db := freshDBWithWebchat(t)
	store := postgres.NewWebchatSessionStore(db.RuntimePool())
	err := store.Touch(context.Background(), uuid.Must(uuid.NewV7()).String())
	if !errors.Is(err, webchat.ErrSessionNotFound) {
		t.Fatalf("Touch(unknown) err = %v, want ErrSessionNotFound", err)
	}
}

// TestMigration0119_DownReverses proves the migration has a working
// backward-compatible step (reversibility AC): the down SQL drops the
// columns and revokes the grants without error after the up applied.
func TestMigration0119_DownReverses(t *testing.T) {
	db := freshDBWithWebchat(t) // applies 0119 up
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	body, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0119_webchat_tenant_origins.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply 0119 down: %v", err)
	}
	// The webchat columns must be gone.
	var n int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name = 'tenants'
		    AND column_name IN ('webchat_allowed_origins','webchat_origin_secret')`).Scan(&n); err != nil {
		t.Fatalf("introspect columns: %v", err)
	}
	if n != 0 {
		t.Fatalf("after down, %d webchat columns remain, want 0", n)
	}
}
