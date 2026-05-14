package postgres_test

// SIN-62725 acceptance criteria for 0089_wallet_basic:
//   #1 migrations up/down idempotent in CI
//   #2 RLS policies on token_wallet, courtesy_grant, and the wallet-aware
//      lanes of token_ledger isolate by tenant
//   #3 UNIQUE(wallet_id, idempotency_key) rejects double-insert
//   #4 100 concurrent INSERTs with the same idempotency_key → 1 success +
//      99 conflicts, balance consistent
//
// Pattern follows inbox_contacts_migration_test.go (SIN-62724): the
// shared harness's DB(t) brings up the cluster + 0001-0003, and this file
// applies 0004 (tenants), 0005 (users), and 0089 on top.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

// walletTableNames lists every table created by 0089_wallet_basic.up.sql.
// token_ledger is excluded because it pre-exists in 0003; the down
// migration restores it to that earlier shape, it is never dropped.
var walletTableNames = []string{
	"token_wallet",
	"courtesy_grant",
}

// freshDBWithWallet applies 0004 (tenants), 0005 (users), and 0089 on top
// of the harness default 0001-0003. Mirrors freshDBWithInboxContacts /
// freshDBWithMasterMFA.
func freshDBWithWallet(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0089_wallet_basic.up.sql",
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

func walletTablesPresent(t *testing.T, ctx context.Context, db *testpg.DB) int {
	t.Helper()
	var count int
	row := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relname = ANY($1)
		    AND n.nspname = 'public'`, walletTableNames)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("table-exists probe: %v", err)
	}
	return count
}

// seedWalletFor creates a token_wallet row for tenantID via the master
// pool (with audit GUC) and returns its id. Master-ops path matches how
// PR11 will provision wallets at tenant creation.
func seedWalletFor(t *testing.T, ctx context.Context, db *testpg.DB, tenantID uuid.UUID, actorID uuid.UUID) uuid.UUID {
	t.Helper()
	var walletID uuid.UUID
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), actorID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO token_wallet (tenant_id, balance, reserved)
			 VALUES ($1, 0, 0) RETURNING id`, tenantID).Scan(&walletID)
	}); err != nil {
		t.Fatalf("seed wallet for %s: %v", tenantID, err)
	}
	return walletID
}

// ---------------------------------------------------------------------------
// AC #1 — up/down idempotency
// ---------------------------------------------------------------------------

// TestWalletMigration_UpDownUp proves both directions of 0089 are
// idempotent and round-trip safe, and that token_ledger columns return
// to their 0003 shape on down.
func TestWalletMigration_UpDownUp(t *testing.T) {
	db := freshDBWithWallet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if got := walletTablesPresent(t, ctx, db); got != len(walletTableNames) {
		t.Fatalf("after initial up: got %d/%d wallet tables", got, len(walletTableNames))
	}
	assertLedgerHasWalletColumns(t, ctx, db, true)

	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0089_wallet_basic.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if got := walletTablesPresent(t, ctx, db); got != 0 {
		t.Fatalf("after down: %d/%d wallet tables still present", got, len(walletTableNames))
	}
	assertLedgerHasWalletColumns(t, ctx, db, false)

	// Re-up confirms forward-roll on the same DB. Tests CI's roll-forward
	// behaviour after a hot-fix down/up cycle.
	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0089_wallet_basic.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if got := walletTablesPresent(t, ctx, db); got != len(walletTableNames) {
		t.Fatalf("after re-up: got %d/%d wallet tables", got, len(walletTableNames))
	}
	assertLedgerHasWalletColumns(t, ctx, db, true)

	// Idempotency: down twice, then up twice. Down-on-already-down and
	// up-on-already-up must both be no-ops without erroring.
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

func assertLedgerHasWalletColumns(t *testing.T, ctx context.Context, db *testpg.DB, want bool) {
	t.Helper()
	var n int
	row := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name = 'token_ledger'
		    AND column_name IN ('wallet_id', 'idempotency_key', 'external_ref', 'created_at')`)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count ledger columns: %v", err)
	}
	if want && n != 4 {
		t.Fatalf("token_ledger wallet columns: have %d, want 4", n)
	}
	if !want && n != 0 {
		t.Fatalf("token_ledger wallet columns leaked after down: have %d, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// AC #2 — RLS policies isolate by tenant
// ---------------------------------------------------------------------------

// TestWalletRLS_TokenWalletTenantIsolation: with the GUC set to tenant A,
// the runtime pool sees only A's wallet row. Mirrors the canonical RLS
// regression from ADR 0072 §process #2.
func TestWalletRLS_TokenWalletTenantIsolation(t *testing.T) {
	db := freshDBWithWallet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "tenantB", fmt.Sprintf("b-%s.crm.local", tenantB)); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	seedWalletFor(t, ctx, db, tenantA, masterID)
	seedWalletFor(t, ctx, db, tenantB, masterID)

	var seenTenants []uuid.UUID
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id FROM token_wallet`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tid uuid.UUID
			if err := rows.Scan(&tid); err != nil {
				return err
			}
			seenTenants = append(seenTenants, tid)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("WithTenant(A): %v", err)
	}

	if len(seenTenants) != 1 || seenTenants[0] != tenantA {
		t.Fatalf("tenant A sees %v, want [%s]", seenTenants, tenantA)
	}
}

// TestWalletRLS_NoTenantSetReturnsZero: the runtime pool without a
// WithTenant scope sees zero wallet rows. Canonical fail-closed check.
func TestWalletRLS_NoTenantSetReturnsZero(t *testing.T) {
	db := freshDBWithWallet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	seedWalletFor(t, ctx, db, tenantA, masterID)

	var n int
	if err := db.RuntimePool().QueryRow(ctx, `SELECT count(*) FROM token_wallet`).Scan(&n); err != nil {
		t.Fatalf("count token_wallet: %v", err)
	}
	if n != 0 {
		t.Errorf("runtime pool with no GUC saw %d wallet rows, want 0", n)
	}

	if err := db.RuntimePool().QueryRow(ctx, `SELECT count(*) FROM courtesy_grant`).Scan(&n); err != nil {
		t.Fatalf("count courtesy_grant: %v", err)
	}
	if n != 0 {
		t.Errorf("runtime pool with no GUC saw %d courtesy_grant rows, want 0", n)
	}
}

// TestWalletRLS_CourtesyGrantTenantIsolation: the runtime can read its
// own grant via the SELECT policy, but not other tenants' grants.
func TestWalletRLS_CourtesyGrantTenantIsolation(t *testing.T) {
	db := freshDBWithWallet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "tenantB", fmt.Sprintf("b-%s.crm.local", tenantB)); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO courtesy_grant (tenant_id, amount, note)
			 VALUES ($1, 1000, 'onboarding-A'), ($2, 500, 'onboarding-B')`,
			tenantA, tenantB); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("seed grants: %v", err)
	}

	var seenNote string
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT note FROM courtesy_grant`)
		if err != nil {
			return err
		}
		defer rows.Close()
		seen := []string{}
		for rows.Next() {
			var note string
			if err := rows.Scan(&note); err != nil {
				return err
			}
			seen = append(seen, note)
		}
		if len(seen) != 1 {
			t.Fatalf("tenant A sees %d grant rows, want 1: %v", len(seen), seen)
		}
		seenNote = seen[0]
		return rows.Err()
	}); err != nil {
		t.Fatalf("WithTenant(A): %v", err)
	}
	if seenNote != "onboarding-A" {
		t.Errorf("tenant A saw %q, want onboarding-A", seenNote)
	}
}

// TestWalletRLS_RuntimeCannotWriteCourtesyGrant: courtesy_grant is
// master-issued — runtime has no INSERT/UPDATE/DELETE grant, and the
// REVOKE is the second line of defence behind the missing RLS write
// policies.
func TestWalletRLS_RuntimeCannotWriteCourtesyGrant(t *testing.T) {
	db := freshDBWithWallet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, _ := seedTenantUserMaster(t, db)

	err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO courtesy_grant (tenant_id, amount, note) VALUES ($1, 1, 'evil')`,
			tenantA)
		return e
	})
	if err == nil {
		t.Fatal("expected permission denied for runtime INSERT on courtesy_grant, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		t.Errorf("expected permission-denied error, got: %v", err)
	}
}

// TestWalletRLS_InsertWrongTenantFails: with the GUC set to tenant A, an
// INSERT that names tenant B is rejected by the WITH CHECK clause on
// token_wallet. Protects against body-tampering (ADR 0072 §3).
func TestWalletRLS_InsertWrongTenantFails(t *testing.T) {
	db := freshDBWithWallet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, _ := seedTenantUserMaster(t, db)
	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "tenantB", fmt.Sprintf("b-%s.crm.local", tenantB)); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO token_wallet (tenant_id, balance) VALUES ($1, 0)`,
			tenantB)
		return e
	})
	if err == nil {
		t.Fatal("expected row-level-security violation, got nil")
	}
	if !strings.Contains(err.Error(), "row-level security") {
		t.Errorf("expected row-level security error, got: %v", err)
	}
}

// TestWalletForceRLS_AppliesToOwner: relforcerowsecurity=true on every
// wallet-aware tenanted table. Canary against any future migration that
// drops FORCE (ADR 0072).
func TestWalletForceRLS_AppliesToOwner(t *testing.T) {
	db := freshDBWithWallet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, table := range []string{"token_wallet", "courtesy_grant"} {
		var force bool
		row := db.SuperuserPool().QueryRow(ctx,
			`SELECT relforcerowsecurity FROM pg_class WHERE relname = $1`, table)
		if err := row.Scan(&force); err != nil {
			t.Fatalf("read relforcerowsecurity(%s): %v", table, err)
		}
		if !force {
			t.Errorf("table %s: FORCE ROW LEVEL SECURITY = false (ADR 0072 violation)", table)
		}
	}
}

// ---------------------------------------------------------------------------
// AC #3 — UNIQUE(wallet_id, idempotency_key) rejects double-insert
// ---------------------------------------------------------------------------

// TestLedgerUnique_DoubleInsertRejected: the canonical idempotency
// invariant. Two ledger inserts with the same (wallet_id, idempotency_key)
// pair MUST fail at the database layer.
func TestLedgerUnique_DoubleInsertRejected(t *testing.T) {
	db := freshDBWithWallet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	walletID := seedWalletFor(t, ctx, db, tenantA, masterID)

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO token_ledger (tenant_id, kind, amount, wallet_id, idempotency_key)
		 VALUES ($1, 'reserve', -10, $2, 'op-1')`,
		tenantA, walletID); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO token_ledger (tenant_id, kind, amount, wallet_id, idempotency_key)
		 VALUES ($1, 'reserve', -10, $2, 'op-1')`,
		tenantA, walletID)
	if err == nil {
		t.Fatal("expected unique-violation for (wallet_id, idempotency_key), got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}
}

// TestLedgerWalletIdemPaired_RejectsHalfWalletAware: the paired CHECK
// constraint forbids rows that name a wallet without an idempotency key
// (and vice versa). Catches half-populated wallet-aware writes that
// would otherwise sneak past the partial UNIQUE index.
func TestLedgerWalletIdemPaired_RejectsHalfWalletAware(t *testing.T) {
	db := freshDBWithWallet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	walletID := seedWalletFor(t, ctx, db, tenantA, masterID)

	// wallet_id without idempotency_key.
	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO token_ledger (tenant_id, kind, amount, wallet_id)
		 VALUES ($1, 'reserve', -1, $2)`,
		tenantA, walletID)
	if err == nil {
		t.Fatal("expected check-violation for wallet_id without idempotency_key, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "token_ledger_wallet_idem_paired") {
		t.Errorf("expected paired-check error, got: %v", err)
	}

	// idempotency_key without wallet_id.
	_, err = db.AdminPool().Exec(ctx,
		`INSERT INTO token_ledger (tenant_id, kind, amount, idempotency_key)
		 VALUES ($1, 'reserve', -1, 'op-orphan')`,
		tenantA)
	if err == nil {
		t.Fatal("expected check-violation for idempotency_key without wallet_id, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "token_ledger_wallet_idem_paired") {
		t.Errorf("expected paired-check error, got: %v", err)
	}
}

// TestLedgerWalletKindCheck_RestrictsWalletAwareKinds: wallet-aware
// inserts must use one of reserve/commit/release/grant; legacy NULL
// wallet inserts are unconstrained.
func TestLedgerWalletKindCheck_RestrictsWalletAwareKinds(t *testing.T) {
	db := freshDBWithWallet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	walletID := seedWalletFor(t, ctx, db, tenantA, masterID)

	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO token_ledger (tenant_id, kind, amount, wallet_id, idempotency_key)
		 VALUES ($1, 'topup', 100, $2, 'op-bad-kind')`,
		tenantA, walletID)
	if err == nil {
		t.Fatal("expected check-violation for unknown wallet kind, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "token_ledger_wallet_kind_check") {
		t.Errorf("expected wallet-kind-check error, got: %v", err)
	}

	// Legacy fixture path (wallet_id NULL) still accepts 'topup' so the
	// 0003 RLS-demo tests in withtenant_test.go keep working.
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO token_ledger (tenant_id, kind, amount) VALUES ($1, 'topup', 100)`,
		tenantA); err != nil {
		t.Errorf("legacy-shape insert rejected unexpectedly: %v", err)
	}
}

// TestWalletBalanceNonNegative_Enforced: the CHECK on token_wallet.balance
// is the first-line defence against over-debit races (F30).
func TestWalletBalanceNonNegative_Enforced(t *testing.T) {
	db := freshDBWithWallet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	walletID := seedWalletFor(t, ctx, db, tenantA, masterID)

	_, err := db.AdminPool().Exec(ctx,
		`UPDATE token_wallet SET balance = -1 WHERE id = $1`, walletID)
	if err == nil {
		t.Fatal("expected check-violation for balance < 0, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "check") {
		t.Errorf("expected check-constraint error, got: %v", err)
	}
}

// TestCourtesyGrantUniqueTenant_Enforced: only one courtesy grant per
// tenant — re-running the onboarding flow must not double-allocate.
func TestCourtesyGrantUniqueTenant_Enforced(t *testing.T) {
	db := freshDBWithWallet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)

	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO courtesy_grant (tenant_id, amount, note) VALUES ($1, 1000, 'first')`,
			tenantA)
		return e
	}); err != nil {
		t.Fatalf("first grant: %v", err)
	}

	err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO courtesy_grant (tenant_id, amount, note) VALUES ($1, 500, 'second')`,
			tenantA)
		return e
	})
	if err == nil {
		t.Fatal("expected unique-violation for second courtesy_grant on same tenant, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC #4 — 100 concurrent INSERTs with same idempotency_key → 1 wins
// ---------------------------------------------------------------------------

// TestLedgerIdempotency_ConcurrentInsertsCollapseToOne is the load-bearing
// guarantee for retry-safe LLM commits (F37). One hundred goroutines race
// to insert the same (wallet_id, idempotency_key) row; exactly one wins,
// 99 see unique-violations, and the wallet's running ledger total
// reflects exactly one credit.
func TestLedgerIdempotency_ConcurrentInsertsCollapseToOne(t *testing.T) {
	db := freshDBWithWallet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	walletID := seedWalletFor(t, ctx, db, tenantA, masterID)

	const N = 100
	const amount int64 = 7
	const idem = "race-key-001"

	var wg sync.WaitGroup
	var success atomic.Int64
	var conflicts atomic.Int64
	var unexpected atomic.Int64

	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, err := db.AdminPool().Exec(ctx,
				`INSERT INTO token_ledger (tenant_id, kind, amount, wallet_id, idempotency_key)
				 VALUES ($1, 'commit', $2, $3, $4)`,
				tenantA, amount, walletID, idem)
			if err == nil {
				success.Add(1)
				return
			}
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
				conflicts.Add(1)
				return
			}
			unexpected.Add(1)
			t.Logf("unexpected concurrent-insert error: %v", err)
		}()
	}
	wg.Wait()

	if got := success.Load(); got != 1 {
		t.Errorf("successes = %d, want 1", got)
	}
	if got := conflicts.Load(); got != N-1 {
		t.Errorf("unique-violation conflicts = %d, want %d", got, N-1)
	}
	if got := unexpected.Load(); got != 0 {
		t.Errorf("unexpected errors = %d, want 0", got)
	}

	// The ledger consistency check: the sum of credits for this
	// (wallet_id, idempotency_key) is exactly one row's worth.
	var sum int64
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT COALESCE(SUM(amount), 0)
		   FROM token_ledger
		  WHERE wallet_id = $1 AND idempotency_key = $2`,
		walletID, idem).Scan(&sum); err != nil {
		t.Fatalf("sum ledger: %v", err)
	}
	if sum != amount {
		t.Errorf("ledger sum = %d, want %d (exactly one row's credit)", sum, amount)
	}

	var rows int64
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM token_ledger
		  WHERE wallet_id = $1 AND idempotency_key = $2`,
		walletID, idem).Scan(&rows); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if rows != 1 {
		t.Errorf("ledger row count for idempotency_key = %d, want 1", rows)
	}
}
