// Integration tests for contacts.IdentityRepository (IdentityStore adapter).
// Lives in package postgres_test so it shares the TestMain + harness from
// withtenant_test.go — avoids a second test binary that races ALTER ROLE
// on the shared CI cluster (see SIN-62794 / memory note testpg_shared_cluster_race).
package postgres_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/contacts"
)

// freshDBWithIdentity applies the full migration chain needed by the identity
// integration tests: 0004→0005→0088→0092_message_media→0092_identity_link.
func freshDBWithIdentity(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0092_identity_link_assignment_history.up.sql",
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

// seedTenantForIdentity inserts a tenant via app_admin (bypasses RLS).
// tenants.host must be unique, so we embed the UUID in the hostname.
func seedTenantForIdentity(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	tenantID := uuid.New()
	ctx := newCtx(t)
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, 'test', $2)`,
		tenantID, tenantID.String()+".test",
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return tenantID
}

// seedContact inserts a contact + channel identity so Resolve has rows to find.
func seedContact(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID, channel, externalID, displayName string) uuid.UUID {
	t.Helper()
	ctx := newCtx(t)
	contactID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, $3)`,
		contactID, tenantID, displayName,
	); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO contact_channel_identity (tenant_id, contact_id, channel, external_id) VALUES ($1, $2, $3, $4)`,
		tenantID, contactID, channel, externalID,
	); err != nil {
		t.Fatalf("seed contact_channel_identity: %v", err)
	}
	return contactID
}

func newIdentityStore(t *testing.T) (*pgcontacts.IdentityStore, *testpg.DB) {
	t.Helper()
	db := freshDBWithIdentity(t)
	store, err := pgcontacts.NewIdentityStore(db.RuntimePool())
	if err != nil {
		t.Fatalf("NewIdentityStore: %v", err)
	}
	return store, db
}

// TestIdentityStore_Resolve_NewContact verifies that Resolve creates a
// fresh Identity when no match exists.
func TestIdentityStore_Resolve_NewContact(t *testing.T) {
	t.Parallel()
	store, db := newIdentityStore(t)
	tenantID := seedTenantForIdentity(t, db.AdminPool())
	seedContact(t, db.AdminPool(), tenantID, "whatsapp", "+5511999990001", "Alice")

	identity, proposal, err := store.Resolve(context.Background(), tenantID,
		"whatsapp", "+5511999990001", "", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if proposal != nil {
		t.Errorf("expected no MergeProposal, got %+v", proposal)
	}
	if identity == nil {
		t.Fatal("expected non-nil identity")
	}
	if identity.TenantID != tenantID {
		t.Errorf("TenantID: got %v, want %v", identity.TenantID, tenantID)
	}
}

// TestIdentityStore_Resolve_Idempotent verifies that calling Resolve twice
// for the same (channel, externalID) returns the same identity.
func TestIdentityStore_Resolve_Idempotent(t *testing.T) {
	t.Parallel()
	store, db := newIdentityStore(t)
	tenantID := seedTenantForIdentity(t, db.AdminPool())
	seedContact(t, db.AdminPool(), tenantID, "whatsapp", "+5511999990002", "Bob")

	id1, _, err := store.Resolve(context.Background(), tenantID, "whatsapp", "+5511999990002", "", "")
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	id2, _, err := store.Resolve(context.Background(), tenantID, "whatsapp", "+5511999990002", "", "")
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if id1.ID != id2.ID {
		t.Errorf("Resolve not idempotent: got %v then %v", id1.ID, id2.ID)
	}
}

// TestIdentityStore_Merge verifies that Merge repoints links and marks
// the source as merged.
func TestIdentityStore_Merge(t *testing.T) {
	t.Parallel()
	store, db := newIdentityStore(t)
	tenantID := seedTenantForIdentity(t, db.AdminPool())

	// Create two contacts that will each get their own identity via backfill.
	seedContact(t, db.AdminPool(), tenantID, "whatsapp", "+5511999990010", "Carol")
	seedContact(t, db.AdminPool(), tenantID, "whatsapp", "+5511999990011", "Dave")

	id1, _, err := store.Resolve(context.Background(), tenantID, "whatsapp", "+5511999990010", "", "")
	if err != nil {
		t.Fatalf("Resolve id1: %v", err)
	}
	id2, _, err := store.Resolve(context.Background(), tenantID, "whatsapp", "+5511999990011", "", "")
	if err != nil {
		t.Fatalf("Resolve id2: %v", err)
	}

	if err := store.Merge(context.Background(), tenantID, id2.ID, id1.ID, "test merge"); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// Reload id1 — it should now have 2 links.
	merged, _, err := store.Resolve(context.Background(), tenantID, "whatsapp", "+5511999990010", "", "")
	if err != nil {
		t.Fatalf("Resolve after merge: %v", err)
	}
	if merged.ID != id1.ID {
		t.Errorf("surviving identity: got %v, want %v", merged.ID, id1.ID)
	}
}

// TestIdentityStore_Split verifies that Split creates a fresh identity
// for the split contact.
func TestIdentityStore_Split(t *testing.T) {
	t.Parallel()
	store, db := newIdentityStore(t)
	tenantID := seedTenantForIdentity(t, db.AdminPool())
	seedContact(t, db.AdminPool(), tenantID, "whatsapp", "+5511999990020", "Eve")

	identity, _, err := store.Resolve(context.Background(), tenantID, "whatsapp", "+5511999990020", "", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(identity.Links) == 0 {
		t.Fatal("expected at least one link after Resolve")
	}
	linkID := identity.Links[0].ID

	if err := store.Split(context.Background(), tenantID, linkID); err != nil {
		t.Fatalf("Split: %v", err)
	}

	// After split, Resolve should give a different identity.
	after, _, err := store.Resolve(context.Background(), tenantID, "whatsapp", "+5511999990020", "", "")
	if err != nil {
		t.Fatalf("Resolve after split: %v", err)
	}
	if after.ID == identity.ID {
		t.Errorf("Split did not create a new identity; still got %v", identity.ID)
	}
}

// TestIdentityStore_Split_NotFound verifies ErrNotFound for unknown linkID.
func TestIdentityStore_Split_NotFound(t *testing.T) {
	t.Parallel()
	store, db := newIdentityStore(t)
	tenantID := seedTenantForIdentity(t, db.AdminPool())
	err := store.Split(context.Background(), tenantID, uuid.New())
	if !isErrContaining(err, contacts.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestNewIdentityStore_NilPool verifies constructor guard.
func TestNewIdentityStore_NilPool(t *testing.T) {
	t.Parallel()
	_, err := pgcontacts.NewIdentityStore(nil)
	if err == nil {
		t.Fatal("expected error for nil pool")
	}
}

// isErrContaining unwraps the error chain looking for target.
func isErrContaining(err, target error) bool {
	if err == nil {
		return false
	}
	// Walk the chain manually since errors.Is works here too.
	for e := err; e != nil; {
		if e == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := e.(unwrapper); ok {
			e = u.Unwrap()
		} else {
			break
		}
	}
	return false
}
