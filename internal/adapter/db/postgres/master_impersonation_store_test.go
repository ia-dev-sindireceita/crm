package postgres_test

// Integration tests for ImpersonationStore (SIN-63971 coverage bar).
//
// Lives in the parent postgres_test package (mastersession pattern) to
// share the cluster harness from withtenant_test.go.
//
// Covers: constructor guard, Start happy path, Start duplicate → ErrAlreadyActive,
// ActiveForSession happy path, ActiveForSession no session → ErrNoActiveImpersonation,
// End happy path, ListAuditByCorrelation (empty).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	masterpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/master"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/iam/impersonation"
)

func freshDBWithImpersonation(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0083_split_audit_log.up.sql",
		"0087_master_session.up.sql",
		"0116_master_impersonation_session.up.sql",
		"0117_audit_log_security_correlation_id.up.sql",
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

// insertMasterOpUser inserts a user and master_session row; returns
// (userID, masterSessionID) for use as StartInput fields.
func insertMasterOpUser(t *testing.T, db *testpg.DB) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var userID uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`INSERT INTO users (email, password_hash, role, is_master)
		 VALUES ($1,'x','master_operator',true) RETURNING id`,
		"testmaster-"+uuid.New().String()+"@example.com",
	).Scan(&userID); err != nil {
		t.Fatalf("insertMasterOpUser: %v", err)
	}
	var msID uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`INSERT INTO master_session (user_id, expires_at)
		 VALUES ($1, now() + interval '1 hour') RETURNING id`, userID,
	).Scan(&msID); err != nil {
		t.Fatalf("insertMasterSession: %v", err)
	}
	return userID, msID
}

// insertMasterTenantForImpersonation creates a minimal tenant row.
func insertMasterTenantForImpersonation(t *testing.T, db *testpg.DB) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.AdminPool().QueryRow(context.Background(),
		`INSERT INTO tenants (name, host) VALUES ($1,$2) RETURNING id`,
		"ImperTenant-"+uuid.New().String(), uuid.New().String()+".example.com",
	).Scan(&id); err != nil {
		t.Fatalf("insertMasterTenantForImpersonation: %v", err)
	}
	return id
}

func TestImpersonationStore_NilPoolReturnsError(t *testing.T) {
	if _, err := masterpg.NewImpersonationStore(nil); err == nil {
		t.Error("nil pool: want error, got nil")
	}
}

func TestImpersonationStore_Start_HappyPath(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, err := masterpg.NewImpersonationStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewImpersonationStore: %v", err)
	}
	userID, msID := insertMasterOpUser(t, db)
	tenantID := insertMasterTenantForImpersonation(t, db)

	sess, err := store.Start(context.Background(), impersonation.StartInput{
		MasterUserID:    userID,
		MasterSessionID: msID,
		TargetTenantID:  tenantID,
		Reason:          "test impersonation reason",
		StartedAt:       time.Now(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sess.ID == uuid.Nil {
		t.Error("sess.ID is nil")
	}
	if sess.TargetTenantID != tenantID {
		t.Errorf("TargetTenantID=%s, want %s", sess.TargetTenantID, tenantID)
	}
}

func TestImpersonationStore_Start_Duplicate_ReturnsErrAlreadyActive(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, err := masterpg.NewImpersonationStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewImpersonationStore: %v", err)
	}
	userID, msID := insertMasterOpUser(t, db)
	tenantID := insertMasterTenantForImpersonation(t, db)

	in := impersonation.StartInput{
		MasterUserID:    userID,
		MasterSessionID: msID,
		TargetTenantID:  tenantID,
		Reason:          "first start reason",
		StartedAt:       time.Now(),
	}
	if _, err := store.Start(context.Background(), in); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	_, err = store.Start(context.Background(), in)
	if !errors.Is(err, impersonation.ErrAlreadyActive) {
		t.Errorf("duplicate Start: want ErrAlreadyActive, got %v", err)
	}
}

func TestImpersonationStore_ActiveForSession_HappyPath(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, err := masterpg.NewImpersonationStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewImpersonationStore: %v", err)
	}
	userID, msID := insertMasterOpUser(t, db)
	tenantID := insertMasterTenantForImpersonation(t, db)

	if _, err := store.Start(context.Background(), impersonation.StartInput{
		MasterUserID:    userID,
		MasterSessionID: msID,
		TargetTenantID:  tenantID,
		Reason:          "active session test",
		StartedAt:       time.Now(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sess, err := store.ActiveForSession(context.Background(), msID)
	if err != nil {
		t.Fatalf("ActiveForSession: %v", err)
	}
	if sess.TargetTenantID != tenantID {
		t.Errorf("TargetTenantID=%s, want %s", sess.TargetTenantID, tenantID)
	}
}

func TestImpersonationStore_ActiveForSession_NoSession(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, err := masterpg.NewImpersonationStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewImpersonationStore: %v", err)
	}

	_, err = store.ActiveForSession(context.Background(), uuid.New())
	if !errors.Is(err, impersonation.ErrNoActiveImpersonation) {
		t.Errorf("want ErrNoActiveImpersonation, got %v", err)
	}
}

func TestImpersonationStore_End_HappyPath(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, err := masterpg.NewImpersonationStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewImpersonationStore: %v", err)
	}
	userID, msID := insertMasterOpUser(t, db)
	tenantID := insertMasterTenantForImpersonation(t, db)

	sess, err := store.Start(context.Background(), impersonation.StartInput{
		MasterUserID:    userID,
		MasterSessionID: msID,
		TargetTenantID:  tenantID,
		Reason:          "end test reason",
		StartedAt:       time.Now(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	err = store.End(context.Background(), sess.ID, userID, "logout", time.Now())
	if err != nil {
		t.Fatalf("End: %v", err)
	}

	// After End, ActiveForSession must return ErrNoActiveImpersonation.
	_, err = store.ActiveForSession(context.Background(), msID)
	if !errors.Is(err, impersonation.ErrNoActiveImpersonation) {
		t.Errorf("after End: want ErrNoActiveImpersonation, got %v", err)
	}
}

func TestImpersonationStore_ListAuditByCorrelation_Empty(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, err := masterpg.NewImpersonationStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewImpersonationStore: %v", err)
	}

	rows, err := store.ListAuditByCorrelation(context.Background(), uuid.New(), 10)
	if err != nil {
		t.Fatalf("ListAuditByCorrelation: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows, want 0", len(rows))
	}
}

func TestImpersonationStore_End_NilID_ReturnsErrNoActiveImpersonation(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, err := masterpg.NewImpersonationStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewImpersonationStore: %v", err)
	}
	userID, _ := insertMasterOpUser(t, db)

	err = store.End(context.Background(), uuid.Nil, userID, "end reason", time.Now())
	if !errors.Is(err, impersonation.ErrNoActiveImpersonation) {
		t.Errorf("nil id: want ErrNoActiveImpersonation, got %v", err)
	}
}

func TestImpersonationStore_End_AlreadyEnded_ReturnsErrNoActiveImpersonation(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, err := masterpg.NewImpersonationStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewImpersonationStore: %v", err)
	}
	userID, msID := insertMasterOpUser(t, db)
	tenantID := insertMasterTenantForImpersonation(t, db)

	sess, err := store.Start(context.Background(), impersonation.StartInput{
		MasterUserID: userID, MasterSessionID: msID,
		TargetTenantID: tenantID, Reason: "double end test reason",
		StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// End once.
	if err := store.End(context.Background(), sess.ID, userID, "first end", time.Now()); err != nil {
		t.Fatalf("first End: %v", err)
	}
	// End again → already ended.
	err = store.End(context.Background(), sess.ID, userID, "second end", time.Now())
	if !errors.Is(err, impersonation.ErrNoActiveImpersonation) {
		t.Errorf("double End: want ErrNoActiveImpersonation, got %v", err)
	}
}

func TestImpersonationStore_ListAuditByCorrelation_NilID_ReturnsNil(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, err := masterpg.NewImpersonationStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewImpersonationStore: %v", err)
	}
	rows, err := store.ListAuditByCorrelation(context.Background(), uuid.Nil, 10)
	if err != nil {
		t.Fatalf("ListAuditByCorrelation: %v", err)
	}
	if rows != nil {
		t.Errorf("want nil, got %v", rows)
	}
}

func TestImpersonationStore_ListAuditByCorrelation_ZeroLimit(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, err := masterpg.NewImpersonationStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewImpersonationStore: %v", err)
	}
	// limit=0 → defaults to 200; should still return empty without error.
	rows, err := store.ListAuditByCorrelation(context.Background(), uuid.New(), 0)
	if err != nil {
		t.Fatalf("ListAuditByCorrelation: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows, want 0", len(rows))
	}
}

func TestImpersonationStore_Start_ShortReason_ReturnsErrInvalidReason(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, err := masterpg.NewImpersonationStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewImpersonationStore: %v", err)
	}
	userID, msID := insertMasterOpUser(t, db)
	tenantID := insertMasterTenantForImpersonation(t, db)

	_, err = store.Start(context.Background(), impersonation.StartInput{
		MasterUserID:    userID,
		MasterSessionID: msID,
		TargetTenantID:  tenantID,
		Reason:          "short", // < 8 chars → CHECK violation → ErrInvalidReason
		StartedAt:       time.Now(),
	})
	if !errors.Is(err, impersonation.ErrInvalidReason) {
		t.Errorf("short reason: want ErrInvalidReason, got %v", err)
	}
}

func TestImpersonationStore_ListAuditByCorrelation_WithRows(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, err := masterpg.NewImpersonationStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewImpersonationStore: %v", err)
	}
	userID, msID := insertMasterOpUser(t, db)
	tenantID := insertMasterTenantForImpersonation(t, db)

	sess, err := store.Start(context.Background(), impersonation.StartInput{
		MasterUserID:    userID,
		MasterSessionID: msID,
		TargetTenantID:  tenantID,
		Reason:          "audit feed test reason",
		StartedAt:       time.Now(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Insert an audit_log_security row with correlation_id = session.ID.
	ctx := context.Background()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO audit_log_security (tenant_id, actor_user_id, event_type, target, occurred_at, correlation_id)
		 VALUES ($1, $2, 'impersonation_start', '{}', now(), $3)`,
		tenantID, userID, sess.ID,
	); err != nil {
		t.Fatalf("insert audit row: %v", err)
	}

	rows, err := store.ListAuditByCorrelation(ctx, sess.ID, 10)
	if err != nil {
		t.Fatalf("ListAuditByCorrelation: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("got %d rows, want 1", len(rows))
	}
}

func TestImpersonationStore_Start_ZeroMasterUser_ReturnsError(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, _ := masterpg.NewImpersonationStore(db.MasterOpsPool())

	_, err := store.Start(context.Background(), impersonation.StartInput{
		MasterUserID: uuid.Nil, MasterSessionID: uuid.New(), TargetTenantID: uuid.New(),
		Reason: "zero user reason",
	})
	if err == nil {
		t.Error("nil MasterUserID: want error, got nil")
	}
}

func TestImpersonationStore_Start_ZeroSessionID_ReturnsError(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, _ := masterpg.NewImpersonationStore(db.MasterOpsPool())

	_, err := store.Start(context.Background(), impersonation.StartInput{
		MasterUserID: uuid.New(), MasterSessionID: uuid.Nil, TargetTenantID: uuid.New(),
		Reason: "zero session reason",
	})
	if err == nil {
		t.Error("nil MasterSessionID: want error, got nil")
	}
}

func TestImpersonationStore_Start_ZeroTenantID_ReturnsError(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, _ := masterpg.NewImpersonationStore(db.MasterOpsPool())

	_, err := store.Start(context.Background(), impersonation.StartInput{
		MasterUserID: uuid.New(), MasterSessionID: uuid.New(), TargetTenantID: uuid.Nil,
		Reason: "zero tenant reason",
	})
	if err == nil {
		t.Error("nil TargetTenantID: want error, got nil")
	}
}

func TestImpersonationStore_Start_ZeroStartedAt_DefaultsToNow(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, err := masterpg.NewImpersonationStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewImpersonationStore: %v", err)
	}
	userID, msID := insertMasterOpUser(t, db)
	tenantID := insertMasterTenantForImpersonation(t, db)

	before := time.Now().Add(-time.Second)
	sess, err := store.Start(context.Background(), impersonation.StartInput{
		MasterUserID: userID, MasterSessionID: msID, TargetTenantID: tenantID,
		Reason: "zero startedat test",
		// StartedAt is zero → should default to now()
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sess.StartedAt.Before(before) {
		t.Errorf("StartedAt=%v is before %v", sess.StartedAt, before)
	}
}

func TestImpersonationStore_End_ZeroActor_ReturnsError(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, _ := masterpg.NewImpersonationStore(db.MasterOpsPool())

	err := store.End(context.Background(), uuid.New(), uuid.Nil, "reason", time.Now())
	if err == nil {
		t.Error("nil actor: want error, got nil")
	}
}

func TestImpersonationStore_ActiveForSession_NilID_ReturnsErrNoActiveImpersonation(t *testing.T) {
	db := freshDBWithImpersonation(t)
	store, _ := masterpg.NewImpersonationStore(db.MasterOpsPool())

	_, err := store.ActiveForSession(context.Background(), uuid.Nil)
	if !errors.Is(err, impersonation.ErrNoActiveImpersonation) {
		t.Errorf("nil id: want ErrNoActiveImpersonation, got %v", err)
	}
}
