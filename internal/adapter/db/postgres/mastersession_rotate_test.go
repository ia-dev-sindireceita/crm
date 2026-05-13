package postgres_test

// SIN-62377 (FAIL-4) integration tests for Store.RotateID. Cover:
//   - Happy path: new id, fields preserved, old id removed atomically.
//   - Missing oldID returns ErrSessionNotFound.
//   - uuid.Nil oldID returns ErrSessionNotFound (defensive guard).
//   - Audit trigger fires on the rotation pair (insert + delete count
//     two rows in master_ops_audit).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/mastersession"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
)

func TestStore_RotateID_HappyPath_PreservesFieldsAndDeletesOldRow(t *testing.T) {
	db := freshDBWithMasterSession(t)
	actor := seedMasterUser(t, db, "actor-rot@master.test")
	user := seedMasterUser(t, db, "user-rot@master.test")
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)
	base, err := mastersession.New(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := base.WithClock(frozenClock(now))

	orig, err := s.Create(ctx, user, time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Stamp mfa_verified_at to assert the rotation preserves it.
	if _, err := s.MarkVerified(ctx, orig.ID); err != nil {
		t.Fatalf("MarkVerified: %v", err)
	}
	preRot, err := s.Get(ctx, orig.ID)
	if err != nil {
		t.Fatalf("Get pre-rotation: %v", err)
	}

	rotated, err := s.RotateID(ctx, orig.ID)
	if err != nil {
		t.Fatalf("RotateID: %v", err)
	}
	if rotated.ID == uuid.Nil {
		t.Fatal("rotated id is uuid.Nil")
	}
	if rotated.ID == orig.ID {
		t.Fatalf("rotated id %s == original id %s — must change", rotated.ID, orig.ID)
	}
	if rotated.UserID != user {
		t.Fatalf("UserID = %s, want %s", rotated.UserID, user)
	}
	if !rotated.CreatedAt.Equal(preRot.CreatedAt) {
		t.Fatalf("CreatedAt = %v, want preserved %v", rotated.CreatedAt, preRot.CreatedAt)
	}
	if !rotated.ExpiresAt.Equal(preRot.ExpiresAt) {
		t.Fatalf("ExpiresAt = %v, want preserved %v", rotated.ExpiresAt, preRot.ExpiresAt)
	}
	if rotated.MFAVerifiedAt == nil || preRot.MFAVerifiedAt == nil ||
		!rotated.MFAVerifiedAt.Equal(*preRot.MFAVerifiedAt) {
		t.Fatalf("MFAVerifiedAt = %v, want preserved %v",
			rotated.MFAVerifiedAt, preRot.MFAVerifiedAt)
	}

	// Old row must be gone.
	if _, err := s.Get(ctx, orig.ID); !errors.Is(err, mastermfa.ErrSessionNotFound) {
		t.Fatalf("Get(old id) err = %v, want ErrSessionNotFound", err)
	}
	// New row must be reachable.
	got, err := s.Get(ctx, rotated.ID)
	if err != nil {
		t.Fatalf("Get(new id): %v", err)
	}
	if got.ID != rotated.ID {
		t.Fatalf("Get(new id).ID = %s, want %s", got.ID, rotated.ID)
	}
}

func TestStore_RotateID_NilOldID_ReturnsErrSessionNotFound(t *testing.T) {
	db := freshDBWithMasterSession(t)
	actor := seedMasterUser(t, db, "actor-rot-nil@master.test")
	s, err := mastersession.New(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.RotateID(context.Background(), uuid.Nil); !errors.Is(err, mastermfa.ErrSessionNotFound) {
		t.Fatalf("RotateID(uuid.Nil) err = %v, want ErrSessionNotFound", err)
	}
}

func TestStore_RotateID_MissingID_ReturnsErrSessionNotFound(t *testing.T) {
	db := freshDBWithMasterSession(t)
	actor := seedMasterUser(t, db, "actor-rot-miss@master.test")
	s, err := mastersession.New(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.RotateID(context.Background(), uuid.New()); !errors.Is(err, mastermfa.ErrSessionNotFound) {
		t.Fatalf("RotateID(missing) err = %v, want ErrSessionNotFound", err)
	}
}

// The master_ops_audit_trigger from migration 0002 fires on every
// row-level INSERT / UPDATE / DELETE. RotateID is INSERT-new + DELETE-
// old, so two new audit rows MUST appear vs. the pre-rotation count.
func TestStore_RotateID_EmitsAuditRowsForInsertAndDelete(t *testing.T) {
	db := freshDBWithMasterSession(t)
	actor := seedMasterUser(t, db, "actor-rot-audit@master.test")
	user := seedMasterUser(t, db, "user-rot-audit@master.test")
	ctx := context.Background()

	s, err := mastersession.New(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	orig, err := s.Create(ctx, user, time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	pre := countAuditRowsForTable(t, db, "master_session")
	if _, err := s.RotateID(ctx, orig.ID); err != nil {
		t.Fatalf("RotateID: %v", err)
	}
	post := countAuditRowsForTable(t, db, "master_session")
	if post-pre < 2 {
		t.Fatalf("audit rows added = %d, want >= 2 (insert+delete)", post-pre)
	}
}

// countAuditRowsForTable counts master_ops_audit rows for the given
// target_table (the trigger writes the row's table name into
// target_table — see migrations/0002_master_ops_audit.up.sql).
func countAuditRowsForTable(t *testing.T, db *testpg.DB, table string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM master_ops_audit WHERE target_table = $1`, table,
	).Scan(&n); err != nil {
		t.Fatalf("count master_ops_audit: %v", err)
	}
	return n
}
