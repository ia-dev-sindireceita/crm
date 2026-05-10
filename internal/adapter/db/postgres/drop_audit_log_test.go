package postgres_test

// SIN-62424 (Phase B.2): up/down/up cycle for migration 0015, which
// drops the legacy `audit_log` table after the SplitAuditLogger took
// over and the LGPD purge job (SIN-62424 Phase B.1) was wired against
// the split tables.
//
// The down step must be a real rollback: it recreates the table with
// the schema 0007 produced + reapplies the 0009 grant on the legacy
// app_audit role. This test asserts both directions so an operator
// running `migrate down` against a Phase-B.2 deployment lands on the
// pre-Phase-B.2 schema (table back, grants back, RLS back, trigger
// back).

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

func TestDropAuditLogMigration_UpDownUp(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	ctx := newCtx(t)

	// freshDBWithSplitAudit applies 4→5→6→7→9→12→13→14, so audit_log
	// is present at this point. Apply 0015 up to drop it.
	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0015_drop_audit_log.up.sql"))
	if err != nil {
		t.Fatalf("read 0015 up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("apply 0015 up: %v", err)
	}
	if tableExists(t, ctx, db.DB, "audit_log") {
		t.Fatal("audit_log still present after 0015 up")
	}
	// Up twice is idempotent (DROP TABLE IF EXISTS).
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("apply 0015 up (idempotent): %v", err)
	}

	// 0015 down must restore the table, the indices, the RLS policies,
	// the master_ops_audit trigger, and the legacy app_audit grant.
	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0015_drop_audit_log.down.sql"))
	if err != nil {
		t.Fatalf("read 0015 down: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply 0015 down: %v", err)
	}
	if !tableExists(t, ctx, db.DB, "audit_log") {
		t.Fatal("audit_log missing after 0015 down — rollback is incomplete")
	}
	// Down twice is idempotent (CREATE IF NOT EXISTS).
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply 0015 down (idempotent): %v", err)
	}

	// Re-up must drop again.
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply 0015 up: %v", err)
	}
	if tableExists(t, ctx, db.DB, "audit_log") {
		t.Fatal("audit_log still present after re-up — drop is non-idempotent")
	}
}

// TestDropAuditLogMigration_DownRestoresLegacyAppAuditGrant asserts
// the rollback is REAL: an operator running 0015 down expects the
// legacy `INSERT ON audit_log TO app_audit` grant from 0009 to be
// back in place, not just the empty table. Without the grant, a
// roll-back deployment would lose the audit-write capability and
// crash the impersonation middleware on the next request.
func TestDropAuditLogMigration_DownRestoresLegacyAppAuditGrant(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	ctx := newCtx(t)

	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0015_drop_audit_log.up.sql"))
	if err != nil {
		t.Fatalf("read 0015 up: %v", err)
	}
	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0015_drop_audit_log.down.sql"))
	if err != nil {
		t.Fatalf("read 0015 down: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("apply 0015 up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply 0015 down: %v", err)
	}

	if !grantExists(t, ctx, db.DB, "app_audit", "audit_log", "INSERT") {
		t.Fatal("app_audit lost INSERT on audit_log after 0015 down — rollback is broken")
	}
	// 0009 only granted INSERT, so SELECT MUST NOT come back. A leaked
	// SELECT would be an over-restoration that breaks least-privilege.
	if grantExists(t, ctx, db.DB, "app_audit", "audit_log", "SELECT") {
		t.Fatal("app_audit gained SELECT on audit_log after 0015 down — rollback over-restored")
	}
	// And the trigger must be back so master_ops writes get audited.
	if !triggerExists(t, ctx, db.DB, "audit_log", "audit_log_master_ops_audit") {
		t.Fatal("audit_log_master_ops_audit trigger missing after 0015 down — rollback is incomplete")
	}
}

// grantExists asks information_schema whether `role` has `priv` on
// `table` in the public schema. The information_schema view is the
// load-bearing source of truth for grants; using has_table_privilege()
// would be answer-ish but loops back through the role-membership
// graph. For migration tests we want the literal grant, not the
// effective privilege.
func grantExists(t *testing.T, ctx context.Context, db *testpg.DB, role, table, priv string) bool {
	t.Helper()
	var got bool
	if err := db.SuperuserPool().QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1
		  FROM information_schema.role_table_grants
		  WHERE table_schema = 'public'
		    AND grantee      = $1
		    AND table_name   = $2
		    AND privilege_type = $3
		)`, role, table, priv).Scan(&got); err != nil {
		t.Fatalf("grant probe: %v", err)
	}
	return got
}

// triggerExists asks pg_trigger whether a named trigger is attached
// to a table. Migrations attach the master_ops_audit trigger to every
// audited table; rollback tests need to confirm the attachment came
// back, not just the table.
func triggerExists(t *testing.T, ctx context.Context, db *testpg.DB, table, trigger string) bool {
	t.Helper()
	var got bool
	if err := db.SuperuserPool().QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1
		  FROM pg_trigger tg
		  JOIN pg_class c ON c.oid = tg.tgrelid
		  WHERE c.relname = $1
		    AND tg.tgname = $2
		    AND NOT tg.tgisinternal
		)`, table, trigger).Scan(&got); err != nil {
		t.Fatalf("trigger probe: %v", err)
	}
	return got
}
