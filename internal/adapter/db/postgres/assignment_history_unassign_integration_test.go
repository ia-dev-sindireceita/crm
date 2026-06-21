// Integration tests for the SIN-65480 unassign event row: migration 0124
// (nullable user_id + 'unassign' reason + presence CHECK) and the
// *pginbox.Store.AppendUnassign write path / nullable read paths. Shares
// the postgres_test TestMain harness and the assignment_history seed
// helpers.
package postgres_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/inbox"
)

// freshDBWithUnassign applies the assignment_history chain PLUS migration
// 0124 so the unassign event row is representable.
func freshDBWithUnassign(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0092_identity_link_assignment_history.up.sql",
		"0124_assignment_history_unassign.up.sql",
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

// monotoneClock returns strictly-increasing UTC timestamps so the latest
// row is unambiguous regardless of wall-clock resolution.
func monotoneClock(start time.Time) func() time.Time {
	cur := start
	return func() time.Time {
		cur = cur.Add(time.Second)
		return cur
	}
}

func newUnassignStore(t *testing.T) (*pginbox.Store, *testpg.DB) {
	t.Helper()
	db := freshDBWithUnassign(t)
	store, err := pginbox.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("inbox.New: %v", err)
	}
	return store.WithClock(monotoneClock(time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC))), db
}

func TestAssignmentRepository_AppendUnassign_Persists(t *testing.T) {
	store, db := newUnassignStore(t)
	tenantID := seedTenantForAssignment(t, db.AdminPool())
	contactID := seedContactForAssignment(t, db.AdminPool(), tenantID)
	convID := seedConversationForAssignment(t, db.AdminPool(), tenantID, contactID)
	userID := seedUserForAssignment(t, db.AdminPool(), tenantID)

	ctx := newCtx(t)
	// First a real assignment, then the unassign event.
	if _, err := store.AppendHistory(ctx, tenantID, convID, userID, inbox.LeadReasonManual); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	ua, err := store.AppendUnassign(ctx, tenantID, convID)
	if err != nil {
		t.Fatalf("AppendUnassign: %v", err)
	}
	if ua.UserID != uuid.Nil {
		t.Errorf("returned UserID = %v, want uuid.Nil", ua.UserID)
	}
	if ua.Reason != inbox.LeadReasonUnassign {
		t.Errorf("returned Reason = %q, want %q", ua.Reason, inbox.LeadReasonUnassign)
	}

	// LatestAssignment must return the unassign row with a nil user (the
	// nullable-scan path) — "current leader = nobody".
	got, err := store.LatestAssignment(ctx, tenantID, convID)
	if err != nil {
		t.Fatalf("LatestAssignment: %v", err)
	}
	if got.ID != ua.ID {
		t.Errorf("latest ID = %v, want the unassign row %v", got.ID, ua.ID)
	}
	if got.UserID != uuid.Nil {
		t.Errorf("latest UserID = %v, want uuid.Nil (NULL user_id)", got.UserID)
	}
	if got.Reason != inbox.LeadReasonUnassign {
		t.Errorf("latest Reason = %q, want %q", got.Reason, inbox.LeadReasonUnassign)
	}

	// ListHistory must include both rows oldest-first, the unassign last and
	// with a nil user — exercising the nullable scan in the list path too.
	rows, err := store.ListHistory(ctx, tenantID, convID)
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("history len = %d, want 2", len(rows))
	}
	if rows[0].UserID != userID || rows[0].Reason != inbox.LeadReasonManual {
		t.Errorf("row[0] = %+v, want manual assignment to %v", rows[0], userID)
	}
	if rows[1].UserID != uuid.Nil || rows[1].Reason != inbox.LeadReasonUnassign {
		t.Errorf("row[1] = %+v, want unassign event with nil user", rows[1])
	}
}

func TestAssignmentRepository_AppendUnassign_NilArgsRejected(t *testing.T) {
	store, _ := newUnassignStore(t)
	ctx := newCtx(t)
	if _, err := store.AppendUnassign(ctx, uuid.Nil, uuid.New()); err == nil {
		t.Errorf("AppendUnassign with nil tenant id was accepted")
	}
	if _, err := store.AppendUnassign(ctx, uuid.New(), uuid.Nil); err == nil {
		t.Errorf("AppendUnassign with nil conversation id was accepted")
	}
}

func TestAssignmentHistory_PresenceCheck_RejectsBadRows(t *testing.T) {
	db := freshDBWithUnassign(t)
	tenantID := seedTenantForAssignment(t, db.AdminPool())
	contactID := seedContactForAssignment(t, db.AdminPool(), tenantID)
	convID := seedConversationForAssignment(t, db.AdminPool(), tenantID, contactID)
	userID := seedUserForAssignment(t, db.AdminPool(), tenantID)
	ctx := newCtx(t)

	// unassign reason WITH a user_id violates the presence CHECK.
	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO assignment_history (id, tenant_id, conversation_id, user_id, reason)
		 VALUES ($1, $2, $3, $4, 'unassign')`,
		uuid.New(), tenantID, convID, userID,
	)
	if err == nil {
		t.Errorf("insert of unassign row WITH a user_id was accepted; presence CHECK should reject it")
	}

	// a non-unassign reason with a NULL user_id violates the presence CHECK.
	_, err = db.AdminPool().Exec(ctx,
		`INSERT INTO assignment_history (id, tenant_id, conversation_id, user_id, reason)
		 VALUES ($1, $2, $3, NULL, 'manual')`,
		uuid.New(), tenantID, convID,
	)
	if err == nil {
		t.Errorf("insert of manual row with NULL user_id was accepted; presence CHECK should reject it")
	}

	// the well-formed unassign row (NULL user_id) is accepted.
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO assignment_history (id, tenant_id, conversation_id, user_id, reason)
		 VALUES ($1, $2, $3, NULL, 'unassign')`,
		uuid.New(), tenantID, convID,
	); err != nil {
		t.Errorf("well-formed unassign row rejected: %v", err)
	}
}
