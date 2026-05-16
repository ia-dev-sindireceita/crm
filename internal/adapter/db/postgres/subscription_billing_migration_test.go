package postgres_test

// SIN-62875 / Fase 2.5 C1 acceptance for 0097_subscription_plan_invoice_master_grant:
//
//   #1 up/down/up idempotent on the shared CI cluster
//   #2 RLS policies on subscription / invoice / master_grant isolate by tenant
//   #3 subscription partial UNIQUE rejects a second active row per tenant
//   #4 invoice partial UNIQUE allows a fresh pending row after a master cancel,
//      rejects a second active row in the same period
//   #5 master_grant CHECK constraints (reason length, revoke consistency
//      including revoked_by_user_id) — ADR-0098 §D1
//   #6 token_ledger.source CHECK + master_grant_id pairing
//   #7 master_grant_request 4-eyes state machine — ADR-0098 §D5
//
// Tests live in the parent postgres_test package (not a per-table subpackage
// or a new test binary) so they share the shared-cluster ALTER ROLE
// bootstrap and don't race the SQLSTATE 28P01 regression pattern from
// SIN-62726 / SIN-62750 (see wallet_basic_migration_test.go for the same
// reason).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

// billingTableNames lists every table created by 0097. token_ledger is
// excluded because it pre-exists (0003); the down migration restores its
// 0089 shape, the table itself is never dropped.
var billingTableNames = []string{
	"plan",
	"subscription",
	"invoice",
	"master_grant",
	"master_grant_request",
}

// freshDBWithBilling applies 0004 (tenants), 0005 (users), 0089
// (wallet_basic — extends token_ledger), and 0097 on top of the harness
// default 0001-0003. Mirrors freshDBWithWallet but stops at the billing
// schema so individual tests can probe ledger behaviour with or without
// the additional updated_at trigger.
func freshDBWithBilling(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0089_wallet_basic.up.sql",
		"0097_subscription_plan_invoice_master_grant.up.sql",
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

func billingTablesPresent(t *testing.T, ctx context.Context, db *testpg.DB) int {
	t.Helper()
	var count int
	row := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relname = ANY($1)
		    AND n.nspname = 'public'`, billingTableNames)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("billing-tables probe: %v", err)
	}
	return count
}

func ledgerHasSourceColumn(t *testing.T, ctx context.Context, db *testpg.DB) bool {
	t.Helper()
	var n int
	row := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name = 'token_ledger' AND column_name = 'source'`)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count source column: %v", err)
	}
	return n == 1
}

// seedPlan inserts a single plan row via AdminPool and returns its id.
func seedPlan(t *testing.T, ctx context.Context, db *testpg.DB, slug string, quota int64) uuid.UUID {
	t.Helper()
	var planID uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`INSERT INTO plan (slug, name, price_cents_brl, monthly_token_quota)
		 VALUES ($1, $2, 0, $3) RETURNING id`, slug, slug, quota).Scan(&planID); err != nil {
		t.Fatalf("seed plan %s: %v", slug, err)
	}
	return planID
}

// seedActiveSubscription inserts an active subscription via master_ops
// (mirroring how the C5 renewer will create them) and returns its id.
func seedActiveSubscription(t *testing.T, ctx context.Context, db *testpg.DB, tenantID, planID, masterID uuid.UUID) uuid.UUID {
	t.Helper()
	var subID uuid.UUID
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO subscription
			   (tenant_id, plan_id, status, current_period_start, current_period_end)
			 VALUES ($1, $2, 'active', now(), now() + interval '30 days')
			 RETURNING id`, tenantID, planID).Scan(&subID)
	}); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	return subID
}

// ---------------------------------------------------------------------------
// AC #1 — up/down idempotency
// ---------------------------------------------------------------------------

// TestBillingMigration_UpDownUp proves both directions of 0097 are
// idempotent and round-trip safe, and that token_ledger returns to its
// 0089 shape on down (no `source` / `master_grant_id` leakage).
func TestBillingMigration_UpDownUp(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if got := billingTablesPresent(t, ctx, db); got != len(billingTableNames) {
		t.Fatalf("after initial up: got %d/%d billing tables", got, len(billingTableNames))
	}
	if !ledgerHasSourceColumn(t, ctx, db) {
		t.Fatalf("token_ledger.source missing after up")
	}

	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(),
		"0097_subscription_plan_invoice_master_grant.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if got := billingTablesPresent(t, ctx, db); got != 0 {
		t.Fatalf("after down: %d/%d billing tables still present", got, len(billingTableNames))
	}
	if ledgerHasSourceColumn(t, ctx, db) {
		t.Fatalf("token_ledger.source leaked after down")
	}

	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(),
		"0097_subscription_plan_invoice_master_grant.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if got := billingTablesPresent(t, ctx, db); got != len(billingTableNames) {
		t.Fatalf("after re-up: got %d/%d billing tables", got, len(billingTableNames))
	}

	// Down-twice and up-twice must both be no-ops without error.
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down (idempotent): %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("apply up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("apply up (idempotent): %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC #2 — RLS policies isolate by tenant
// ---------------------------------------------------------------------------

// TestBillingRLS_SubscriptionTenantIsolation: runtime under WithTenant(A)
// sees only A's subscription.
func TestBillingRLS_SubscriptionTenantIsolation(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "tenantB", fmt.Sprintf("b-%s.crm.local", tenantB)); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	planID := seedPlan(t, ctx, db, "pro", 1_000_000)
	seedActiveSubscription(t, ctx, db, tenantA, planID, masterID)
	seedActiveSubscription(t, ctx, db, tenantB, planID, masterID)

	var seen []uuid.UUID
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id FROM subscription`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tid uuid.UUID
			if err := rows.Scan(&tid); err != nil {
				return err
			}
			seen = append(seen, tid)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("WithTenant(A): %v", err)
	}
	if len(seen) != 1 || seen[0] != tenantA {
		t.Fatalf("tenant A sees %v, want [%s]", seen, tenantA)
	}
}

// TestBillingRLS_NoTenantSetReturnsZero: runtime pool without a WithTenant
// scope sees zero rows on every tenanted billing table. Canonical
// fail-closed check (ADR-0072).
func TestBillingRLS_NoTenantSetReturnsZero(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	planID := seedPlan(t, ctx, db, "pro", 1_000_000)
	subID := seedActiveSubscription(t, ctx, db, tenantA, planID, masterID)

	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO invoice
			   (tenant_id, subscription_id, period_start, period_end,
			    amount_cents_brl, state)
			 VALUES ($1, $2, current_date, current_date + 30, 9900, 'pending')`,
			tenantA, subID)
		return e
	}); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}

	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO master_grant
			   (external_id, tenant_id, created_by_user_id, kind, reason, payload)
			 VALUES ('01HXXX0CHECKZEROBYRUNTIME', $1, $2,
			         'free_subscription_period',
			         'no-tenant-guc smoke check',
			         '{"months": 1, "plan_slug": "free"}'::jsonb)`,
			tenantA, masterID)
		return e
	}); err != nil {
		t.Fatalf("seed master_grant: %v", err)
	}

	for _, table := range []string{"subscription", "invoice", "master_grant"} {
		var n int
		q := fmt.Sprintf(`SELECT count(*) FROM %s`, table)
		if err := db.RuntimePool().QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("runtime pool with no GUC saw %d %s rows, want 0", n, table)
		}
	}
}

// TestBillingRLS_RuntimeCannotWriteSubscription: subscription writes are
// reserved to master_ops; runtime has no INSERT grant.
func TestBillingRLS_RuntimeCannotWriteSubscription(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, _ := seedTenantUserMaster(t, db)
	planID := seedPlan(t, ctx, db, "pro", 1_000_000)

	err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO subscription
			   (tenant_id, plan_id, status, current_period_start, current_period_end)
			 VALUES ($1, $2, 'active', now(), now() + interval '30 days')`,
			tenantA, planID)
		return e
	})
	if err == nil {
		t.Fatal("expected permission denied for runtime INSERT on subscription, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		t.Errorf("expected permission-denied error, got: %v", err)
	}
}

// TestBillingForceRLS_AppliesToOwner: relforcerowsecurity=true on every
// tenanted billing table. Canary against any future migration that
// forgets FORCE.
func TestBillingForceRLS_AppliesToOwner(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, table := range []string{"subscription", "invoice", "master_grant", "master_grant_request"} {
		var force bool
		row := db.SuperuserPool().QueryRow(ctx,
			`SELECT relforcerowsecurity FROM pg_class WHERE relname = $1`, table)
		if err := row.Scan(&force); err != nil {
			t.Fatalf("read relforcerowsecurity(%s): %v", table, err)
		}
		if !force {
			t.Errorf("table %s: FORCE ROW LEVEL SECURITY = false (ADR-0072 violation)", table)
		}
	}
}

// ---------------------------------------------------------------------------
// AC #3 — subscription one-active-per-tenant
// ---------------------------------------------------------------------------

// TestSubscriptionPartialUnique_OneActivePerTenant: the partial UNIQUE
// rejects a second `active` subscription for the same tenant; a
// `cancelled` row in the same tenant does NOT block a fresh active one.
func TestSubscriptionPartialUnique_OneActivePerTenant(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	planID := seedPlan(t, ctx, db, "pro", 1_000_000)
	seedActiveSubscription(t, ctx, db, tenantA, planID, masterID)

	// Second active for the same tenant must be rejected.
	err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO subscription
			   (tenant_id, plan_id, status, current_period_start, current_period_end)
			 VALUES ($1, $2, 'active', now(), now() + interval '30 days')`,
			tenantA, planID)
		return e
	})
	if err == nil {
		t.Fatal("expected unique-violation for second active subscription, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}

	// Flip the existing row to cancelled; a fresh active one is now
	// allowed alongside the cancelled history row.
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`UPDATE subscription SET status = 'cancelled' WHERE tenant_id = $1`,
			tenantA)
		return e
	}); err != nil {
		t.Fatalf("cancel subscription: %v", err)
	}

	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO subscription
			   (tenant_id, plan_id, status, current_period_start, current_period_end)
			 VALUES ($1, $2, 'active', now(), now() + interval '30 days')`,
			tenantA, planID)
		return e
	}); err != nil {
		t.Errorf("fresh active subscription after cancel rejected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC #4 — invoice partial UNIQUE (renewer idempotency)
// ---------------------------------------------------------------------------

// TestInvoicePartialUnique_OneActivePerPeriod: two pending invoices for
// the same (tenant, period_start) collide; cancelling the first frees
// the slot for a new pending invoice in the same period.
func TestInvoicePartialUnique_OneActivePerPeriod(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	planID := seedPlan(t, ctx, db, "pro", 1_000_000)
	subID := seedActiveSubscription(t, ctx, db, tenantA, planID, masterID)

	insertInvoice := func(state, reason string) error {
		var args []any
		var stmt string
		switch state {
		case "cancelled_by_master":
			stmt = `INSERT INTO invoice
			   (tenant_id, subscription_id, period_start, period_end,
			    amount_cents_brl, state, cancelled_reason)
			 VALUES ($1, $2, date '2026-06-01', date '2026-07-01', 9900,
			         'cancelled_by_master', $3)`
			args = []any{tenantA, subID, reason}
		default:
			stmt = `INSERT INTO invoice
			   (tenant_id, subscription_id, period_start, period_end,
			    amount_cents_brl, state)
			 VALUES ($1, $2, date '2026-06-01', date '2026-07-01', 9900, $3)`
			args = []any{tenantA, subID, state}
		}
		return postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
			_, e := tx.Exec(ctx, stmt, args...)
			return e
		})
	}

	if err := insertInvoice("pending", ""); err != nil {
		t.Fatalf("first pending invoice: %v", err)
	}
	err := insertInvoice("pending", "")
	if err == nil {
		t.Fatal("expected unique-violation for second pending invoice in same period, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}

	// Cancel the first invoice (master action, reason ≥ 10 chars). The
	// partial UNIQUE excludes cancelled rows, so a fresh pending invoice
	// for the SAME period is now legal — this is the "operator unstuck"
	// flow from plan-doc §3.
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`UPDATE invoice
			    SET state = 'cancelled_by_master',
			        cancelled_reason = 'operator override 2026-06'
			  WHERE tenant_id = $1 AND period_start = date '2026-06-01'`,
			tenantA)
		return e
	}); err != nil {
		t.Fatalf("cancel invoice: %v", err)
	}

	if err := insertInvoice("pending", ""); err != nil {
		t.Errorf("fresh pending invoice after master-cancel rejected: %v", err)
	}
}

// TestInvoiceCancelledReason_RequiresMinLength: the paired CHECK forces
// `cancelled_reason` to be NULL on non-cancelled rows and ≥10 chars on
// cancelled rows. ADR-0098 / SecurityEngineer audit invariant.
func TestInvoiceCancelledReason_RequiresMinLength(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	planID := seedPlan(t, ctx, db, "pro", 1_000_000)
	subID := seedActiveSubscription(t, ctx, db, tenantA, planID, masterID)

	// Cancelled with too-short reason: rejected.
	err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO invoice
			   (tenant_id, subscription_id, period_start, period_end,
			    amount_cents_brl, state, cancelled_reason)
			 VALUES ($1, $2, date '2026-07-01', date '2026-08-01', 9900,
			         'cancelled_by_master', 'short')`,
			tenantA, subID)
		return e
	})
	if err == nil {
		t.Fatal("expected check-violation for short cancelled_reason, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "invoice_cancelled_reason_required") {
		t.Errorf("expected invoice_cancelled_reason_required error, got: %v", err)
	}

	// Cancelled with NULL reason: rejected. Regression: an early draft
	// of the CHECK relied on char_length(NULL) propagating as UNKNOWN,
	// which Postgres treats as not-violating — a NULL reason would
	// slip through. The IS NOT NULL guard in the constraint forces a
	// concrete reason.
	err = postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO invoice
			   (tenant_id, subscription_id, period_start, period_end,
			    amount_cents_brl, state, cancelled_reason)
			 VALUES ($1, $2, date '2026-09-15', date '2026-10-15', 9900,
			         'cancelled_by_master', NULL)`,
			tenantA, subID)
		return e
	})
	if err == nil {
		t.Fatal("expected check-violation for NULL cancelled_reason, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "invoice_cancelled_reason_required") {
		t.Errorf("expected invoice_cancelled_reason_required error, got: %v", err)
	}

	// Pending with a stray reason: rejected.
	err = postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO invoice
			   (tenant_id, subscription_id, period_start, period_end,
			    amount_cents_brl, state, cancelled_reason)
			 VALUES ($1, $2, date '2026-08-01', date '2026-09-01', 9900,
			         'pending', 'unexpected reason on a pending invoice')`,
			tenantA, subID)
		return e
	})
	if err == nil {
		t.Fatal("expected check-violation for cancelled_reason on pending invoice, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "invoice_cancelled_reason_required") {
		t.Errorf("expected invoice_cancelled_reason_required error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC #5 — master_grant CHECK constraints (ADR-0098 §D1)
// ---------------------------------------------------------------------------

// TestMasterGrant_ReasonTooShortRejected: reason must be ≥ 10 chars
// (ADR-0098 §D1: prevents the worst class of bad audit entry — "ok",
// "fix", empty string).
func TestMasterGrant_ReasonTooShortRejected(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)

	err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO master_grant
			   (external_id, tenant_id, created_by_user_id, kind, reason, payload)
			 VALUES ('01HXXSHORTREASON0000000001', $1, $2,
			         'free_subscription_period', 'too short',
			         '{"months": 1, "plan_slug": "free"}'::jsonb)`,
			tenantA, masterID)
		return e
	})
	if err == nil {
		t.Fatal("expected check-violation for short reason, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "master_grant_reason_check") &&
		!strings.Contains(strings.ToLower(err.Error()), "check constraint") {
		t.Errorf("expected reason check-constraint error, got: %v", err)
	}
}

// TestMasterGrant_PayloadIsOpaqueJSONB: payload is opaque JSONB at the
// schema layer (ADR-0098 §D1 + §D6 — shape validation lives in the
// domain, not the DB). The schema accepts both kind-specific shapes
// the writer is expected to emit; future grant kinds (D7 reservation)
// can extend the JSON without a migration.
func TestMasterGrant_PayloadIsOpaqueJSONB(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)

	cases := []struct {
		name    string
		extID   string
		kind    string
		payload string
	}{
		{
			"extra_tokens tokens-only payload",
			"01HXXPAYLOADTOKENS0000000A",
			"extra_tokens",
			`{"tokens": 100000}`,
		},
		{
			"free_subscription_period months payload",
			"01HXXPAYLOADMONTHS0000000A",
			"free_subscription_period",
			`{"months": 3, "plan_slug": "pro"}`,
		},
		{
			"extra_tokens payload with extra metadata",
			"01HXXPAYLOADEXTRA00000000A",
			"extra_tokens",
			`{"tokens": 50000, "compensates_grant_id": "01HXXCOMPENSATES0000000000"}`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
				_, e := tx.Exec(ctx,
					`INSERT INTO master_grant
					   (external_id, tenant_id, created_by_user_id, kind, reason, payload)
					 VALUES ($1, $2, $3, $4,
					         'reason long enough for the check', $5::jsonb)`,
					tc.extID, tenantA, masterID, tc.kind, tc.payload)
				return e
			})
			if err != nil {
				t.Errorf("%s: rejected: %v", tc.name, err)
			}
		})
	}
}

// TestMasterGrant_RevokeConsistency: master_grant_revoke_consistency
// CHECK from ADR-0098 §D1. revoked_at MUST be paired with
// revoked_by_user_id AND a revoke_reason ≥ 10 chars, AND is only
// legal while consumed_at IS NULL. Five branches the constraint must
// reject; one legal revocation; one revoke-after-consume rejection.
func TestMasterGrant_RevokeConsistency(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)

	// Seed a pending grant we can mutate.
	pendingExt := "01HXXREVOK00000000000000001"
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO master_grant
			   (external_id, tenant_id, created_by_user_id, kind, reason, payload)
			 VALUES ($1, $2, $3,
			         'free_subscription_period',
			         'manual onboarding for staging',
			         '{"months": 1, "plan_slug": "free"}'::jsonb)`,
			pendingExt, tenantA, masterID)
		return e
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	type partial struct {
		name string
		set  string
		args []any
	}
	partials := []partial{
		{
			"revoked_at without by + reason",
			`SET revoked_at = now()`,
			nil,
		},
		{
			"revoked_at + by without reason",
			`SET revoked_at = now(), revoked_by_user_id = $2`,
			[]any{masterID},
		},
		{
			"revoked_at + reason without by",
			`SET revoked_at = now(), revoke_reason = 'master revoked unused grant'`,
			nil,
		},
		{
			"reason too short",
			`SET revoked_at = now(), revoked_by_user_id = $2, revoke_reason = 'short'`,
			[]any{masterID},
		},
		{
			"reason is NULL (UNKNOWN-trap regression)",
			`SET revoked_at = now(), revoked_by_user_id = $2, revoke_reason = NULL`,
			[]any{masterID},
		},
	}
	for _, p := range partials {
		p := p
		t.Run(p.name, func(t *testing.T) {
			err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
				args := append([]any{pendingExt}, p.args...)
				_, e := tx.Exec(ctx,
					`UPDATE master_grant `+p.set+` WHERE external_id = $1`,
					args...)
				return e
			})
			if err == nil {
				t.Fatalf("%s: expected check-violation, got nil", p.name)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "master_grant_revoke_consistency") {
				t.Errorf("%s: expected master_grant_revoke_consistency error, got: %v", p.name, err)
			}
		})
	}

	// Legal revocation: all three fields populated, consumed_at IS NULL.
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`UPDATE master_grant
			    SET revoked_at = now(),
			        revoked_by_user_id = $2,
			        revoke_reason = 'master revoked unused grant'
			  WHERE external_id = $1`,
			pendingExt, masterID)
		return e
	}); err != nil {
		t.Errorf("legal revocation rejected: %v", err)
	}

	// Seed a consumed grant, then try to revoke it: rejected even with
	// all three revoke fields populated (consumed grants are terminal
	// per §D4; only a compensating grant offsets them).
	consumedExt := "01HXXCONSUMED0000000000002"
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO master_grant
			   (external_id, tenant_id, created_by_user_id, kind, reason, payload,
			    consumed_at, consumed_ref)
			 VALUES ($1, $2, $3,
			         'extra_tokens',
			         'bonus tokens already applied',
			         '{"tokens": 5000}'::jsonb,
			         now(),
			         '01HXXLEDGERCONSUMEDREF0001')`,
			consumedExt, tenantA, masterID)
		return e
	}); err != nil {
		t.Fatalf("seed consumed grant: %v", err)
	}

	err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`UPDATE master_grant
			    SET revoked_at = now(),
			        revoked_by_user_id = $2,
			        revoke_reason = 'too late, already consumed'
			  WHERE external_id = $1`,
			consumedExt, masterID)
		return e
	})
	if err == nil {
		t.Fatal("expected check-violation for revoke-after-consume, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "master_grant_revoke_consistency") {
		t.Errorf("expected revoke-consistency error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC #6 — token_ledger.source CHECK + master_grant_id pairing
// ---------------------------------------------------------------------------

// TestTokenLedgerSource_RejectsUnknownValue: only the three documented
// source values are accepted.
func TestTokenLedgerSource_RejectsUnknownValue(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, _ := seedTenantUserMaster(t, db)

	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO token_ledger (tenant_id, kind, amount, source)
		 VALUES ($1, 'topup', 1, 'unknown_source')`, tenantA)
	if err == nil {
		t.Fatal("expected check-violation for unknown source, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "token_ledger_source_check") {
		t.Errorf("expected source-check error, got: %v", err)
	}
}

// TestTokenLedgerSource_MasterGrantPairing: source='master_grant'
// requires master_grant_id; non-master_grant sources MUST have NULL FK.
// master_grant.id is now a UUID (ADR-0098 §D1), so master_grant_id is a
// UUID too — we capture the auto-generated id from the seed INSERT.
func TestTokenLedgerSource_MasterGrantPairing(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	var grantID uuid.UUID
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO master_grant
			   (external_id, tenant_id, created_by_user_id, kind, reason, payload)
			 VALUES ('01HXXLEDGERPAIRING000000001', $1, $2,
			         'extra_tokens',
			         'ledger pairing fixture grant',
			         '{"tokens": 1000}'::jsonb)
			 RETURNING id`,
			tenantA, masterID).Scan(&grantID)
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	// source=master_grant without master_grant_id: rejected.
	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO token_ledger (tenant_id, kind, amount, source)
		 VALUES ($1, 'topup', 1000, 'master_grant')`, tenantA)
	if err == nil {
		t.Fatal("expected check-violation for master_grant without master_grant_id, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "token_ledger_master_grant_pairing") {
		t.Errorf("expected ledger-pairing error, got: %v", err)
	}

	// source=monthly_alloc with a stray master_grant_id: rejected.
	_, err = db.AdminPool().Exec(ctx,
		`INSERT INTO token_ledger (tenant_id, kind, amount, source, master_grant_id)
		 VALUES ($1, 'topup', 1000, 'monthly_alloc', $2)`, tenantA, grantID)
	if err == nil {
		t.Fatal("expected check-violation for monthly_alloc with master_grant_id, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "token_ledger_master_grant_pairing") {
		t.Errorf("expected ledger-pairing error, got: %v", err)
	}

	// source=master_grant with valid FK: accepted.
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO token_ledger (tenant_id, kind, amount, source, master_grant_id)
		 VALUES ($1, 'topup', 1000, 'master_grant', $2)`, tenantA, grantID); err != nil {
		t.Errorf("legal master_grant ledger insert rejected: %v", err)
	}

	// source=consumption with NULL FK: accepted (default path).
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO token_ledger (tenant_id, kind, amount, source)
		 VALUES ($1, 'topup', 1, 'consumption')`, tenantA); err != nil {
		t.Errorf("legal consumption ledger insert rejected: %v", err)
	}
}

// TestTokenLedgerSource_DefaultIsConsumption: legacy callers that don't
// supply `source` get the 'consumption' default. The expand-step
// invariant the next migration may contract once writers are updated.
func TestTokenLedgerSource_DefaultIsConsumption(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, _ := seedTenantUserMaster(t, db)

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO token_ledger (tenant_id, kind, amount)
		 VALUES ($1, 'topup', 42)`, tenantA); err != nil {
		t.Fatalf("legacy-shape insert rejected: %v", err)
	}

	var source string
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT source FROM token_ledger
		  WHERE tenant_id = $1 AND amount = 42`, tenantA).Scan(&source); err != nil {
		t.Fatalf("read source: %v", err)
	}
	if source != "consumption" {
		t.Errorf("default source = %q, want consumption", source)
	}
}

// TestPlanSlugUnique: the plan catalogue's slug UNIQUE constraint
// prevents duplicate seeded plans across re-runs of plans.sql.
func TestPlanSlugUnique(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO plan (slug, name, price_cents_brl, monthly_token_quota)
		 VALUES ('starter', 'Starter', 0, 1)`); err != nil {
		t.Fatalf("first plan insert: %v", err)
	}
	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO plan (slug, name, price_cents_brl, monthly_token_quota)
		 VALUES ('starter', 'Starter Dup', 0, 1)`)
	if err == nil {
		t.Fatal("expected unique-violation for duplicate plan slug, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}
}

// TestPlanSeedFile_Idempotent: applying migrations/seed/plans.sql twice
// in a row must succeed without errors and leave the three seeded plans
// in place. Guards the ON CONFLICT (slug) DO NOTHING contract.
func TestPlanSeedFile_Idempotent(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	seed, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "seed", "plans.sql"))
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.AdminPool().Exec(ctx, string(seed)); err != nil {
			t.Fatalf("apply seed (run %d): %v", i, err)
		}
	}

	rows, err := db.AdminPool().Query(ctx,
		`SELECT slug FROM plan WHERE slug = ANY($1) ORDER BY slug`,
		[]string{"free", "pro", "enterprise"})
	if err != nil {
		t.Fatalf("query plans: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			t.Fatalf("scan slug: %v", err)
		}
		got[slug] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	for _, want := range []string{"free", "pro", "enterprise"} {
		if !got[want] {
			t.Errorf("seed plan %q missing after two applies", want)
		}
	}
}

// ---------------------------------------------------------------------------
// AC #7 — master_grant_request 4-eyes state machine (ADR-0098 §D5)
// ---------------------------------------------------------------------------

// seedSecondMaster inserts a second master user (distinct from masterID)
// so the 4-eyes tests have a legal approver. ADR-0098 §D5 requires the
// approver and requester to be different rows in users.
func seedSecondMaster(t *testing.T, ctx context.Context, db *testpg.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, NULL, $2, 'x', 'master', true)`,
		id, fmt.Sprintf("master2-%s@x", id)); err != nil {
		t.Fatalf("seed second master: %v", err)
	}
	return id
}

// TestMasterGrantRequest_InsertAwaiting: a fresh awaiting_approval request
// is legal with no approver / no decided_at; rejected when those fields
// are pre-set (state machine entry invariant).
func TestMasterGrantRequest_InsertAwaiting(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)

	// Awaiting_approval with no approver / no decided_at: accepted.
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO master_grant_request
			   (external_id, tenant_id, kind, payload, reason,
			    created_by_user_id, state)
			 VALUES ('01HXXREQAWAITING000000001', $1,
			         'extra_tokens',
			         '{"tokens": 50000000}'::jsonb,
			         'over-cap bonus for legacy client onboarding',
			         $2, 'awaiting_approval')`,
			tenantA, masterID)
		return e
	}); err != nil {
		t.Errorf("legal awaiting_approval insert rejected: %v", err)
	}

	// Awaiting_approval with a pre-filled approver: rejected by the
	// state-consistency CHECK.
	approver := seedSecondMaster(t, ctx, db)
	err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO master_grant_request
			   (external_id, tenant_id, kind, payload, reason,
			    created_by_user_id, requires_second_approver_id, state)
			 VALUES ('01HXXREQBADAWAIT0000000001', $1,
			         'extra_tokens',
			         '{"tokens": 50000000}'::jsonb,
			         'over-cap bonus for legacy client onboarding',
			         $2, $3, 'awaiting_approval')`,
			tenantA, masterID, approver)
		return e
	})
	if err == nil {
		t.Fatal("expected check-violation for awaiting_approval with approver, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "master_grant_request_state_consistency") {
		t.Errorf("expected state-consistency error, got: %v", err)
	}
}

// TestMasterGrantRequest_ApproveTransition: an awaiting request can be
// updated to 'approved' only when requires_second_approver_id AND
// decided_at are both populated.
func TestMasterGrantRequest_ApproveTransition(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	approver := seedSecondMaster(t, ctx, db)

	extID := "01HXXREQAPPROVED000000001"
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO master_grant_request
			   (external_id, tenant_id, kind, payload, reason,
			    created_by_user_id, state)
			 VALUES ($1, $2, 'extra_tokens',
			         '{"tokens": 50000000}'::jsonb,
			         'enterprise migration overage credit',
			         $3, 'awaiting_approval')`,
			extID, tenantA, masterID)
		return e
	}); err != nil {
		t.Fatalf("seed request: %v", err)
	}

	// Approve without decided_at: rejected.
	err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`UPDATE master_grant_request
			    SET state = 'approved',
			        requires_second_approver_id = $2
			  WHERE external_id = $1`,
			extID, approver)
		return e
	})
	if err == nil {
		t.Fatal("expected check-violation for approve without decided_at, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "master_grant_request_state_consistency") {
		t.Errorf("expected state-consistency error, got: %v", err)
	}

	// Approve with both approver + decided_at: accepted.
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`UPDATE master_grant_request
			    SET state = 'approved',
			        requires_second_approver_id = $2,
			        decided_at = now()
			  WHERE external_id = $1`,
			extID, approver)
		return e
	}); err != nil {
		t.Errorf("legal approve rejected: %v", err)
	}
}

// TestMasterGrantRequest_ApproverDistinctFromRequester: ADR-0098 §D5
// 4-eyes invariant — the second approver cannot be the requester. The
// CHECK fires on UPDATE when requires_second_approver_id is filled.
func TestMasterGrantRequest_ApproverDistinctFromRequester(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)

	extID := "01HXXREQSELFAPPROVE000001"
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO master_grant_request
			   (external_id, tenant_id, kind, payload, reason,
			    created_by_user_id, state)
			 VALUES ($1, $2, 'extra_tokens',
			         '{"tokens": 50000000}'::jsonb,
			         'attempted self-approval regression check',
			         $3, 'awaiting_approval')`,
			extID, tenantA, masterID)
		return e
	}); err != nil {
		t.Fatalf("seed request: %v", err)
	}

	// requester == approver: rejected by the distinct-approver CHECK.
	err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`UPDATE master_grant_request
			    SET state = 'approved',
			        requires_second_approver_id = $2,
			        decided_at = now()
			  WHERE external_id = $1`,
			extID, masterID)
		return e
	})
	if err == nil {
		t.Fatal("expected check-violation for self-approval, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "master_grant_request_distinct_approver") {
		t.Errorf("expected distinct-approver error, got: %v", err)
	}
}

// TestMasterGrantRequest_RuntimeCannotSelect: master_grant_request is
// master-only — runtime pool has no SELECT grant. ADR-0098 §D5: requests
// are internal master plumbing, not tenant-visible until promoted.
func TestMasterGrantRequest_RuntimeCannotSelect(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO master_grant_request
			   (external_id, tenant_id, kind, payload, reason,
			    created_by_user_id, state)
			 VALUES ('01HXXREQRUNTIMEHIDE00001', $1, 'extra_tokens',
			         '{"tokens": 50000000}'::jsonb,
			         'runtime visibility regression check',
			         $2, 'awaiting_approval')`,
			tenantA, masterID)
		return e
	}); err != nil {
		t.Fatalf("seed request: %v", err)
	}

	err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM master_grant_request`).Scan(&n)
	})
	if err == nil {
		t.Fatal("expected permission denied for runtime SELECT on master_grant_request, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		t.Errorf("expected permission-denied error, got: %v", err)
	}
}
