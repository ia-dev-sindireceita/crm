// Integration tests for the inbox.AssignmentRepository adapter
// (F2-07 / SIN-62793). Lives in package postgres_test to share the
// TestMain harness and avoid a second test binary that would race
// ALTER ROLE on the shared CI cluster (memory note
// testpg_shared_cluster_race).
package postgres_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/inbox"
)

// freshDBWithAssignment applies the migration chain needed by the
// assignment_history integration tests: tenant + users + inbox +
// identity_link/assignment_history.
func freshDBWithAssignment(t *testing.T) *testpg.DB {
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

// seedTenantForAssignment inserts a tenant via app_admin (bypasses RLS).
func seedTenantForAssignment(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
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

// seedUserForAssignment inserts a user under the tenant scope. user_id
// is a FK target for assignment_history.user_id.
func seedUserForAssignment(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := newCtx(t)
	userID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role)
		 VALUES ($1, $2, $3, 'x', 'agent')`,
		userID, tenantID, userID.String()+"@test",
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return userID
}

// seedConversationForAssignment inserts a conversation row that the
// assignment_history FK can point at.
func seedConversationForAssignment(t *testing.T, pool *pgxpool.Pool, tenantID, contactID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := newCtx(t)
	convID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversation (id, tenant_id, contact_id, channel, state)
		 VALUES ($1, $2, $3, 'whatsapp', 'open')`,
		convID, tenantID, contactID,
	); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return convID
}

// seedContactForAssignment inserts a contact row so the conversation
// FK can resolve. Returns the contact UUID.
func seedContactForAssignment(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := newCtx(t)
	cID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, 'test contact')`,
		cID, tenantID,
	); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	return cID
}

func newAssignmentStore(t *testing.T) (*pginbox.Store, *testpg.DB) {
	t.Helper()
	db := freshDBWithAssignment(t)
	store, err := pginbox.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("inbox.New: %v", err)
	}
	return store, db
}

func TestAssignmentRepository_AppendHistory_Persists(t *testing.T) {
	store, db := newAssignmentStore(t)
	tenantID := seedTenantForAssignment(t, db.AdminPool())
	contactID := seedContactForAssignment(t, db.AdminPool(), tenantID)
	convID := seedConversationForAssignment(t, db.AdminPool(), tenantID, contactID)
	userID := seedUserForAssignment(t, db.AdminPool(), tenantID)

	ctx := newCtx(t)
	a, err := store.AppendHistory(ctx, tenantID, convID, userID, inbox.LeadReasonLead)
	if err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	if a.ID == uuid.Nil || a.Reason != inbox.LeadReasonLead || a.UserID != userID {
		t.Errorf("returned assignment = %+v", a)
	}

	// Read it back through LatestAssignment.
	got, err := store.LatestAssignment(ctx, tenantID, convID)
	if err != nil {
		t.Fatalf("LatestAssignment: %v", err)
	}
	if got.ID != a.ID {
		t.Errorf("ID = %v, want %v", got.ID, a.ID)
	}
	if got.UserID != userID {
		t.Errorf("UserID = %v, want %v", got.UserID, userID)
	}
	if got.Reason != inbox.LeadReasonLead {
		t.Errorf("Reason = %q, want %q", got.Reason, inbox.LeadReasonLead)
	}
}

func TestAssignmentRepository_LatestAssignment_NotFound(t *testing.T) {
	store, db := newAssignmentStore(t)
	tenantID := seedTenantForAssignment(t, db.AdminPool())
	contactID := seedContactForAssignment(t, db.AdminPool(), tenantID)
	convID := seedConversationForAssignment(t, db.AdminPool(), tenantID, contactID)

	ctx := newCtx(t)
	_, err := store.LatestAssignment(ctx, tenantID, convID)
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestAssignmentRepository_ListHistory_OldestFirst(t *testing.T) {
	store, db := newAssignmentStore(t)
	tenantID := seedTenantForAssignment(t, db.AdminPool())
	contactID := seedContactForAssignment(t, db.AdminPool(), tenantID)
	convID := seedConversationForAssignment(t, db.AdminPool(), tenantID, contactID)
	u1 := seedUserForAssignment(t, db.AdminPool(), tenantID)
	u2 := seedUserForAssignment(t, db.AdminPool(), tenantID)
	u3 := seedUserForAssignment(t, db.AdminPool(), tenantID)

	ctx := newCtx(t)
	// Pin clock to ensure stable ordering despite the same-second insert
	// risk in fast test runs.
	t0 := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	for i, tup := range []struct {
		user   uuid.UUID
		reason inbox.LeadReason
	}{
		{u1, inbox.LeadReasonLead},
		{u2, inbox.LeadReasonManual},
		{u3, inbox.LeadReasonReassign},
	} {
		clockedStore := store.WithClock(func() time.Time { return t0.Add(time.Duration(i) * time.Second) })
		if _, err := clockedStore.AppendHistory(ctx, tenantID, convID, tup.user, tup.reason); err != nil {
			t.Fatalf("AppendHistory[%d]: %v", i, err)
		}
	}

	rows, err := store.ListHistory(ctx, tenantID, convID)
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d, want 3", len(rows))
	}
	if rows[0].UserID != u1 || rows[1].UserID != u2 || rows[2].UserID != u3 {
		t.Errorf("history order = [%v,%v,%v], want [%v,%v,%v]",
			rows[0].UserID, rows[1].UserID, rows[2].UserID, u1, u2, u3)
	}
	if rows[2].Reason != inbox.LeadReasonReassign {
		t.Errorf("latest reason = %q, want %q", rows[2].Reason, inbox.LeadReasonReassign)
	}
	if !rows[0].AssignedAt.Before(rows[2].AssignedAt) {
		t.Errorf("AssignedAt not strictly increasing: %v vs %v",
			rows[0].AssignedAt, rows[2].AssignedAt)
	}

	// Confirm Latest matches the last row.
	latest, err := store.LatestAssignment(ctx, tenantID, convID)
	if err != nil {
		t.Fatalf("LatestAssignment: %v", err)
	}
	if latest.UserID != u3 {
		t.Errorf("Latest = %v, want %v", latest.UserID, u3)
	}
}

func TestAssignmentRepository_InvalidReason(t *testing.T) {
	store, _ := newAssignmentStore(t)
	ctx := newCtx(t)
	_, err := store.AppendHistory(ctx, uuid.New(), uuid.New(), uuid.New(), inbox.LeadReason("auto"))
	if !errors.Is(err, inbox.ErrInvalidLeadReason) {
		t.Errorf("err = %v, want ErrInvalidLeadReason", err)
	}
}

func TestAssignmentRepository_RLSIsolation(t *testing.T) {
	store, db := newAssignmentStore(t)
	tenantA := seedTenantForAssignment(t, db.AdminPool())
	tenantB := seedTenantForAssignment(t, db.AdminPool())
	contactA := seedContactForAssignment(t, db.AdminPool(), tenantA)
	convA := seedConversationForAssignment(t, db.AdminPool(), tenantA, contactA)
	userA := seedUserForAssignment(t, db.AdminPool(), tenantA)

	ctx := newCtx(t)
	if _, err := store.AppendHistory(ctx, tenantA, convA, userA, inbox.LeadReasonLead); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	// Look it up under tenantB — RLS must hide it as ErrNotFound.
	_, err := store.LatestAssignment(ctx, tenantB, convA)
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("cross-tenant Latest err = %v, want ErrNotFound", err)
	}
	rows, err := store.ListHistory(ctx, tenantB, convA)
	if err != nil {
		t.Fatalf("cross-tenant ListHistory err = %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("cross-tenant ListHistory returned %d rows, want 0", len(rows))
	}
}
