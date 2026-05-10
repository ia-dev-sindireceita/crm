package postgres_test

// SIN-62424: integration tests for the postgres AuditPurgeStore and
// the LGPD retention sweep over audit_log_data.
//
// Tests apply the same migration chain as audit_logger_split_test.go
// (4→5→6→7→9→12→13→14) and exercise PurgeExpired end-to-end through
// the dedicated app_master_ops pool. The AC #3 regression test from
// SIN-62252 lives here: a security row dated 14 months ago survives,
// the matching data row is purged.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/audit/purge"
)

// seedExpirableTenantUser inserts a tenant + a regular user. It mirrors
// seedSplitTenantUser but the labels and identifiers are unique to
// purge tests so traces are easy to follow when a test fails.
func seedExpirableTenantUser(t *testing.T, db *testpg.DB, label string) (tenantID, userID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tenantID = uuid.New()
	userID = uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantID, "purge-"+label, fmt.Sprintf("purge-%s-%s.crm.local", label, tenantID)); err != nil {
		t.Fatalf("seed tenant %s: %v", label, err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role)
		 VALUES ($1, $2, $3, 'x', 'admin')`,
		userID, tenantID, fmt.Sprintf("purge-%s-%s@x", label, userID)); err != nil {
		t.Fatalf("seed user %s: %v", label, err)
	}
	return tenantID, userID
}

// insertSecurity inserts a row directly via app_admin (BYPASSRLS) so
// tests can backdate occurred_at past the split logger's column DEFAULT.
func insertSecurity(t *testing.T, db *testpg.DB, tenantID, userID uuid.UUID, when time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO audit_log_security (tenant_id, actor_user_id, event_type, occurred_at)
		 VALUES ($1, $2, 'login', $3)`,
		tenantID, userID, when); err != nil {
		t.Fatalf("seed audit_log_security: %v", err)
	}
}

func insertData(t *testing.T, db *testpg.DB, tenantID, userID uuid.UUID, when time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO audit_log_data (tenant_id, actor_user_id, event_type, occurred_at)
		 VALUES ($1, $2, 'read_pii', $3)`,
		tenantID, userID, when); err != nil {
		t.Fatalf("seed audit_log_data: %v", err)
	}
}

// monthsAgo returns now - n calendar months. Aligns with the
// `make_interval(months => ...)` semantics the purge SQL uses.
func monthsAgo(n int) time.Time {
	return time.Now().UTC().AddDate(0, -n, 0)
}

// newPurgeSweeper wires the postgres AuditPurgeStore + Sweeper for one
// test. The clock is pinned to the wall clock at sweep time so the
// assertions reason about the same `now` the SQL saw.
func newPurgeSweeper(t *testing.T, db *splitAuditDB) *purge.Sweeper {
	t.Helper()
	store, err := postgresadapter.NewAuditPurgeStore(db.MasterOpsPool(),
		uuid.MustParse(postgresadapter.LGPDPurgeActorID))
	if err != nil {
		t.Fatalf("NewAuditPurgeStore: %v", err)
	}
	sw, err := purge.New(store, func() time.Time { return time.Now().UTC() })
	if err != nil {
		t.Fatalf("purge.New: %v", err)
	}
	return sw
}

func countRows(t *testing.T, db *testpg.DB, sql string, args ...any) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got int
	if err := db.AdminPool().QueryRow(ctx, sql, args...).Scan(&got); err != nil {
		t.Fatalf("count: %v", err)
	}
	return got
}

// ---------------------------------------------------------------------------
// AC #3 regression (SIN-62252): security survives, data is purged.
// ---------------------------------------------------------------------------

func TestAuditPurge_SecurityRowSurvives_DataRowExpires(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	tenantID, userID := seedExpirableTenantUser(t, db.DB, "ac3")
	ctx := newCtx(t)

	// Fourteen months ago: data row is well past the 12-month default
	// retention; security row is past it too but security is supposed
	// to be untouched regardless.
	old := monthsAgo(14)
	insertSecurity(t, db.DB, tenantID, userID, old)
	insertData(t, db.DB, tenantID, userID, old)

	sw := newPurgeSweeper(t, db)
	got, err := sw.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got.DeletedRows != 1 || got.TenantsSwept != 1 {
		t.Fatalf("result=%+v, want {DeletedRows:1 TenantsSwept:1}", got)
	}

	if n := countRows(t, db.DB,
		`SELECT count(*) FROM audit_log_security WHERE tenant_id = $1`, tenantID); n != 1 {
		t.Fatalf("audit_log_security count=%d, want 1 (security must NOT be touched)", n)
	}
	if n := countRows(t, db.DB,
		`SELECT count(*) FROM audit_log_data WHERE tenant_id = $1`, tenantID); n != 0 {
		t.Fatalf("audit_log_data count=%d, want 0 (expired row should be purged)", n)
	}
}

// ---------------------------------------------------------------------------
// Per-tenant retention override is honoured.
// ---------------------------------------------------------------------------

func TestAuditPurge_HonoursTenantOverride(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	tenantID, userID := seedExpirableTenantUser(t, db.DB, "override")
	ctx := newCtx(t)

	// Override retention to 24 months: a row at -18 months should NOT
	// be purged, but a row at -25 months MUST be.
	if _, err := db.AdminPool().Exec(ctx,
		`UPDATE tenants SET audit_data_retention_months = 24 WHERE id = $1`, tenantID); err != nil {
		t.Fatalf("override retention: %v", err)
	}
	insertData(t, db.DB, tenantID, userID, monthsAgo(18))
	insertData(t, db.DB, tenantID, userID, monthsAgo(25))

	sw := newPurgeSweeper(t, db)
	got, err := sw.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got.DeletedRows != 1 || got.TenantsSwept != 1 {
		t.Fatalf("result=%+v, want {DeletedRows:1 TenantsSwept:1}", got)
	}

	// The 18-month row stays.
	if n := countRows(t, db.DB,
		`SELECT count(*) FROM audit_log_data WHERE tenant_id = $1`, tenantID); n != 1 {
		t.Fatalf("audit_log_data after sweep=%d, want 1 (the -18m row should survive 24m retention)", n)
	}
}

// ---------------------------------------------------------------------------
// Cross-tenant: every tenant uses its own retention.
// ---------------------------------------------------------------------------

func TestAuditPurge_AcrossTenants(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	tenantA, userA := seedExpirableTenantUser(t, db.DB, "tenant-a")
	tenantB, userB := seedExpirableTenantUser(t, db.DB, "tenant-b")
	ctx := newCtx(t)

	// Tenant A: default 12 months. Insert one expired (-13m), one fresh
	// (-1m).
	insertData(t, db.DB, tenantA, userA, monthsAgo(13))
	insertData(t, db.DB, tenantA, userA, monthsAgo(1))

	// Tenant B: override to 6 months. Insert one expired (-7m), one
	// fresh (-2m).
	if _, err := db.AdminPool().Exec(ctx,
		`UPDATE tenants SET audit_data_retention_months = 6 WHERE id = $1`, tenantB); err != nil {
		t.Fatalf("override tenant B: %v", err)
	}
	insertData(t, db.DB, tenantB, userB, monthsAgo(7))
	insertData(t, db.DB, tenantB, userB, monthsAgo(2))

	sw := newPurgeSweeper(t, db)
	got, err := sw.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got.DeletedRows != 2 || got.TenantsSwept != 2 {
		t.Fatalf("result=%+v, want {DeletedRows:2 TenantsSwept:2}", got)
	}
	if n := countRows(t, db.DB,
		`SELECT count(*) FROM audit_log_data WHERE tenant_id = $1`, tenantA); n != 1 {
		t.Fatalf("tenant A remaining=%d, want 1", n)
	}
	if n := countRows(t, db.DB,
		`SELECT count(*) FROM audit_log_data WHERE tenant_id = $1`, tenantB); n != 1 {
		t.Fatalf("tenant B remaining=%d, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Empty sweep: no rows / nothing expired returns a zero Result.
// ---------------------------------------------------------------------------

func TestAuditPurge_NoExpiredRows_ReturnsZero(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	tenantID, userID := seedExpirableTenantUser(t, db.DB, "fresh")
	ctx := newCtx(t)

	insertData(t, db.DB, tenantID, userID, monthsAgo(1))

	sw := newPurgeSweeper(t, db)
	got, err := sw.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got.DeletedRows != 0 || got.TenantsSwept != 0 {
		t.Fatalf("result=%+v, want {DeletedRows:0 TenantsSwept:0}", got)
	}
	if n := countRows(t, db.DB,
		`SELECT count(*) FROM audit_log_data WHERE tenant_id = $1`, tenantID); n != 1 {
		t.Fatalf("audit_log_data count=%d, want 1 (nothing should be deleted)", n)
	}
}

// ---------------------------------------------------------------------------
// Master_ops audit trail records every deleted row.
// ---------------------------------------------------------------------------

func TestAuditPurge_LeavesMasterOpsTrail(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	tenantID, userID := seedExpirableTenantUser(t, db.DB, "trail")
	ctx := newCtx(t)

	insertData(t, db.DB, tenantID, userID, monthsAgo(13))
	insertData(t, db.DB, tenantID, userID, monthsAgo(14))

	sw := newPurgeSweeper(t, db)
	if _, err := sw.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// Two delete rows under the LGPD purge actor against
	// audit_log_data.
	expectedActor := uuid.MustParse(postgresadapter.LGPDPurgeActorID)
	var n int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM master_ops_audit
		 WHERE actor_user_id = $1 AND query_kind = 'delete' AND target_table = 'audit_log_data'`,
		expectedActor).Scan(&n); err != nil {
		t.Fatalf("count master_ops_audit: %v", err)
	}
	if n != 2 {
		t.Fatalf("master_ops_audit deletes=%d, want 2", n)
	}
}

// ---------------------------------------------------------------------------
// Constructor / argument validation.
// ---------------------------------------------------------------------------

func TestAuditPurge_NewWithNilPool(t *testing.T) {
	t.Parallel()
	if _, err := postgresadapter.NewAuditPurgeStore(nil, uuid.New()); !errors.Is(err, postgresadapter.ErrNilPool) {
		t.Fatalf("err=%v, want ErrNilPool", err)
	}
}

func TestAuditPurge_NewWithZeroActor(t *testing.T) {
	t.Parallel()
	db := harness.DB(t)
	if _, err := postgresadapter.NewAuditPurgeStore(db.MasterOpsPool(), uuid.Nil); !errors.Is(err, postgresadapter.ErrZeroActor) {
		t.Fatalf("err=%v, want ErrZeroActor", err)
	}
}

func TestAuditPurge_LGPDPurgeActorID_IsParsable(t *testing.T) {
	t.Parallel()
	got, err := uuid.Parse(postgresadapter.LGPDPurgeActorID)
	if err != nil {
		t.Fatalf("parse LGPDPurgeActorID: %v", err)
	}
	if got == uuid.Nil {
		t.Fatal("LGPDPurgeActorID must not be uuid.Nil")
	}
}
