package postgres_test

// SIN-62750 follow-up to PR #80 (SIN-62727).
//
// Wallet adapter integration tests live in the parent postgres_test
// package — not the internal/adapter/db/postgres/wallet subpackage —
// because `go test -race ./...` starts every package's test binary in
// parallel. Each binary that calls testpg.Start() bootstraps the
// SHARED Postgres cluster (CI TEST_DATABASE_URL) by ALTERing the
// app_admin / app_runtime / app_master_ops role passwords to its own
// per-process value. Two binaries racing on that ALTER yield SQLSTATE
// 28P01 (password authentication failed) for whichever bootstrap was
// overwritten — the same regression pattern SIN-62726 fixed for the
// contacts adapter (commit 7d9cf39, contacts_adapter_test.go) and
// that PR #80 inadvertently reintroduced. We follow the contacts
// precedent: tests in parent, adapter code in subpackage.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	walletadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres/wallet"
	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/wallet/usecase"
)

// freshDBWithWalletTrigger builds on freshDBWithWallet (defined in
// wallet_basic_migration_test.go) by also applying 0090
// (token_wallet BEFORE UPDATE → updated_at trigger), and returns a
// seeded tenant + master user pair so the per-test setup is a single
// call.
func freshDBWithWalletTrigger(t *testing.T) (*testpg.DB, uuid.UUID, uuid.UUID) {
	t.Helper()
	db := freshDBWithWallet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	body, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0090_wallet_updated_at_trigger.up.sql"))
	if err != nil {
		t.Fatalf("read 0090: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply 0090: %v", err)
	}
	tenantID := uuid.New()
	masterID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantID, "wallet-test", fmt.Sprintf("wallet-%s.crm.local", tenantID)); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, NULL, $2, 'x', 'master', true)`,
		masterID, fmt.Sprintf("m-%s@x", masterID)); err != nil {
		t.Fatalf("seed master user: %v", err)
	}
	return db, tenantID, masterID
}

// seedWalletWithBalance creates a token_wallet row for tenantID and
// seeds it with `balance` tokens via a direct master-ops INSERT (so
// the running balance starts at the requested value).
func seedWalletWithBalance(t *testing.T, ctx context.Context, db *testpg.DB, tenantID, masterID uuid.UUID, balance int64) uuid.UUID {
	t.Helper()
	var walletID uuid.UUID
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO token_wallet (tenant_id, balance, reserved)
			 VALUES ($1, $2, 0) RETURNING id`, tenantID, balance,
		).Scan(&walletID)
	}); err != nil {
		t.Fatalf("seed wallet: %v", err)
	}
	return walletID
}

// newWalletCtx returns a 60s test-scoped context. Wallet tests use the
// longer timeout (vs the package-level newCtx 30s) because the
// concurrent-reserve race test orchestrates 100 goroutines through the
// adapter's SELECT FOR UPDATE path.
func newWalletCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ---------------------------------------------------------------------------
// Migration 0090 — BEFORE UPDATE trigger refreshes updated_at
// ---------------------------------------------------------------------------

func TestWalletUpdatedAt_TriggerRefreshes(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	walletID := seedWalletWithBalance(t, ctx, db, tenantID, masterID, 100)

	var before time.Time
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT updated_at FROM token_wallet WHERE id = $1`, walletID,
	).Scan(&before); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}

	// Sleep so the trigger's now() has a measurable delta.
	time.Sleep(20 * time.Millisecond)
	if _, err := db.AdminPool().Exec(ctx,
		`UPDATE token_wallet SET balance = balance + 1 WHERE id = $1`, walletID,
	); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}

	var after time.Time
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT updated_at FROM token_wallet WHERE id = $1`, walletID,
	).Scan(&after); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}

	if !after.After(before) {
		t.Errorf("trigger did not refresh updated_at: before=%v after=%v", before, after)
	}
}

func TestWalletUpdatedAtMigration_DownUp(t *testing.T) {
	t.Parallel()
	db, _, _ := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)

	down, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0090_wallet_updated_at_trigger.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(down)); err != nil {
		t.Fatalf("apply down: %v", err)
	}

	// After down, the trigger and function are gone.
	var triggerCount, funcCount int
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_trigger WHERE tgname = 'token_wallet_set_updated_at'`,
	).Scan(&triggerCount); err != nil {
		t.Fatalf("count trigger: %v", err)
	}
	if triggerCount != 0 {
		t.Errorf("trigger lingered after down: %d", triggerCount)
	}
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_proc WHERE proname = 'set_updated_at'`,
	).Scan(&funcCount); err != nil {
		t.Fatalf("count function: %v", err)
	}
	if funcCount != 0 {
		t.Errorf("function lingered after down: %d", funcCount)
	}

	// Re-apply up; idempotent.
	up, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0090_wallet_updated_at_trigger.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(up)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(up)); err != nil {
		t.Fatalf("re-apply up (idempotent): %v", err)
	}
}

// ---------------------------------------------------------------------------
// Adapter wiring — repository constructor + nil rejection
// ---------------------------------------------------------------------------

func TestNewRepository_RejectsNilPool(t *testing.T) {
	t.Parallel()
	if _, err := walletadapter.NewRepository(nil); err == nil {
		t.Fatal("NewRepository(nil): want error, got nil")
	}
}

func TestLoadByTenant_NoWallet(t *testing.T) {
	t.Parallel()
	db, tenantID, _ := freshDBWithWalletTrigger(t)
	repo, err := walletadapter.NewRepository(db.RuntimePool())
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}
	if _, err := repo.LoadByTenant(newWalletCtx(t), tenantID); err != wallet.ErrNotFound && err != nil {
		// Defensive: depending on errors.Is the test should match by
		// equality and chain; both work.
		t.Fatalf("LoadByTenant with no wallet: got %v, want ErrNotFound", err)
	}
}

func TestLoadByTenant_ZeroTenant(t *testing.T) {
	t.Parallel()
	db, _, _ := freshDBWithWalletTrigger(t)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	if _, err := repo.LoadByTenant(newWalletCtx(t), uuid.Nil); err != wallet.ErrZeroTenant {
		t.Fatalf("LoadByTenant(uuid.Nil): got %v, want ErrZeroTenant", err)
	}
}

func TestLoadByTenant_HappyPath(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	walletID := seedWalletWithBalance(t, ctx, db, tenantID, masterID, 100)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	w, err := repo.LoadByTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("LoadByTenant: %v", err)
	}
	if w.ID() != walletID {
		t.Errorf("wallet id = %s, want %s", w.ID(), walletID)
	}
	if w.Balance() != 100 || w.Reserved() != 0 || w.Version() != 0 {
		t.Errorf("loaded state: bal=%d rsv=%d ver=%d, want 100/0/0", w.Balance(), w.Reserved(), w.Version())
	}
}

// ---------------------------------------------------------------------------
// Reserve via the use-case service (covers the full adapter surface
// including ApplyWithLock's SELECT FOR UPDATE + version check).
// ---------------------------------------------------------------------------

func TestService_Reserve_HappyPath(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 100)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	svc, _ := usecase.NewService(repo, nil)

	res, err := svc.Reserve(ctx, tenantID, 40, "op-1")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.Amount != 40 || res.TenantID != tenantID {
		t.Errorf("reservation = %+v", res)
	}

	w, _ := repo.LoadByTenant(ctx, tenantID)
	if w.Balance() != 100 || w.Reserved() != 40 || w.Version() != 1 {
		t.Errorf("post-Reserve state: bal=%d rsv=%d ver=%d, want 100/40/1", w.Balance(), w.Reserved(), w.Version())
	}
}

func TestService_Reserve_Idempotent(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 100)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	svc, _ := usecase.NewService(repo, nil)

	first, err := svc.Reserve(ctx, tenantID, 30, "k")
	if err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	second, err := svc.Reserve(ctx, tenantID, 30, "k")
	if err != nil {
		t.Fatalf("retry Reserve: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("retried reserve allocated a new id: %s vs %s", first.ID, second.ID)
	}
	w, _ := repo.LoadByTenant(ctx, tenantID)
	if w.Reserved() != 30 || w.Version() != 1 {
		t.Errorf("retry double-debited: rsv=%d ver=%d, want 30/1", w.Reserved(), w.Version())
	}
}

func TestService_Reserve_InsufficientFunds(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 10)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	svc, _ := usecase.NewService(repo, nil)

	_, err := svc.Reserve(ctx, tenantID, 50, "k")
	if err != wallet.ErrInsufficientFunds {
		t.Fatalf("Reserve over balance: got %v, want ErrInsufficientFunds", err)
	}
}

// TestService_Reserve_RaceAtomic is AC #2 with REAL Postgres.
// 100 concurrent reserves against a 50-token wallet → 50 successes,
// 50 ErrInsufficientFunds, ledger has 50 reserve rows, balance unchanged.
func TestService_Reserve_RaceAtomic(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	walletID := seedWalletWithBalance(t, ctx, db, tenantID, masterID, 50)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	svc, _ := usecase.NewService(repo, nil)

	const N = 100
	const amount = 1

	var wg sync.WaitGroup
	var success, insufficient, other atomic.Int64
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Reserve(ctx, tenantID, amount, fmt.Sprintf("op-%d", i))
			switch err {
			case nil:
				success.Add(1)
			case wallet.ErrInsufficientFunds:
				insufficient.Add(1)
			default:
				other.Add(1)
				t.Logf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if got := success.Load(); got != 50 {
		t.Errorf("successes = %d, want 50", got)
	}
	if got := insufficient.Load(); got != 50 {
		t.Errorf("ErrInsufficientFunds = %d, want 50", got)
	}
	if got := other.Load(); got != 0 {
		t.Errorf("unexpected errors = %d, want 0", got)
	}

	w, _ := repo.LoadByTenant(ctx, tenantID)
	if w.Balance() != 50 || w.Reserved() != 50 {
		t.Errorf("final wallet state: bal=%d rsv=%d, want 50/50", w.Balance(), w.Reserved())
	}
	// Available = balance - reserved must be exactly 0.
	if w.Available() != 0 {
		t.Errorf("final available = %d, want 0", w.Available())
	}

	// Exactly 50 reserve ledger rows on this wallet.
	var ledgerCount int64
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM token_ledger WHERE wallet_id = $1 AND kind = 'reserve'`, walletID,
	).Scan(&ledgerCount); err != nil {
		t.Fatalf("count reserve ledger rows: %v", err)
	}
	if ledgerCount != 50 {
		t.Errorf("reserve rows = %d, want 50", ledgerCount)
	}
}

// ---------------------------------------------------------------------------
// Commit / Release / Grant via the use-case service against real Postgres.
// ---------------------------------------------------------------------------

func TestService_Commit_HappyPath(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 100)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	svc, _ := usecase.NewService(repo, nil)

	res, err := svc.Reserve(ctx, tenantID, 50, "rsv")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := svc.Commit(ctx, res, 30, "cmt"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	w, _ := repo.LoadByTenant(ctx, tenantID)
	if w.Balance() != 70 || w.Reserved() != 0 || w.Version() != 2 {
		t.Errorf("post-Commit state: bal=%d rsv=%d ver=%d, want 70/0/2", w.Balance(), w.Reserved(), w.Version())
	}

	// Idempotent retry.
	if err := svc.Commit(ctx, res, 30, "cmt"); err != nil {
		t.Fatalf("retry Commit: %v", err)
	}
	w, _ = repo.LoadByTenant(ctx, tenantID)
	if w.Balance() != 70 {
		t.Errorf("retry double-debited: bal=%d", w.Balance())
	}
}

func TestService_Release_HappyPath(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 100)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	svc, _ := usecase.NewService(repo, nil)

	res, err := svc.Reserve(ctx, tenantID, 50, "rsv")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := svc.Release(ctx, res, "rel"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	w, _ := repo.LoadByTenant(ctx, tenantID)
	if w.Balance() != 100 || w.Reserved() != 0 {
		t.Errorf("post-Release state: bal=%d rsv=%d, want 100/0", w.Balance(), w.Reserved())
	}
}

func TestService_Grant_HappyPath(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 0)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	svc, _ := usecase.NewService(repo, nil)

	if err := svc.Grant(ctx, tenantID, 500, "g", "src"); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	w, _ := repo.LoadByTenant(ctx, tenantID)
	if w.Balance() != 500 {
		t.Errorf("post-Grant balance = %d, want 500", w.Balance())
	}

	// Idempotent.
	if err := svc.Grant(ctx, tenantID, 500, "g", "src"); err != nil {
		t.Fatalf("retry Grant: %v", err)
	}
	w, _ = repo.LoadByTenant(ctx, tenantID)
	if w.Balance() != 500 {
		t.Errorf("retry double-credited: bal=%d", w.Balance())
	}
}

// ---------------------------------------------------------------------------
// RLS — cross-tenant reads collapse to ErrNotFound.
// ---------------------------------------------------------------------------

func TestLoadByTenant_CrossTenantHidden(t *testing.T) {
	t.Parallel()
	db, tenantA, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantA, masterID, 100)

	// Seed a second tenant + its wallet.
	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "wallet-B", fmt.Sprintf("b-%s.crm.local", tenantB)); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}
	seedWalletWithBalance(t, ctx, db, tenantB, masterID, 7)

	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	// Reading tenant A returns A's wallet.
	wa, err := repo.LoadByTenant(ctx, tenantA)
	if err != nil {
		t.Fatalf("LoadByTenant(A): %v", err)
	}
	if wa.Balance() != 100 {
		t.Errorf("tenant A balance = %d, want 100 (RLS leaking?)", wa.Balance())
	}
}

// ---------------------------------------------------------------------------
// LookupCompletedByExternalRef — settled reservation detection.
// ---------------------------------------------------------------------------

func TestLookupCompletedByExternalRef_AfterCommit(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 100)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	svc, _ := usecase.NewService(repo, nil)

	res, _ := svc.Reserve(ctx, tenantID, 50, "rsv")
	if err := svc.Commit(ctx, res, 50, "cmt"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	entry, err := repo.LookupCompletedByExternalRef(ctx, tenantID, res.WalletID, res.ID.String())
	if err != nil {
		t.Fatalf("LookupCompletedByExternalRef: %v", err)
	}
	if entry.Kind != wallet.KindCommit {
		t.Errorf("settled entry kind = %s, want commit", entry.Kind)
	}
}

func TestLookupCompletedByExternalRef_WhileOpen(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 100)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	svc, _ := usecase.NewService(repo, nil)

	res, _ := svc.Reserve(ctx, tenantID, 50, "rsv")
	_, err := repo.LookupCompletedByExternalRef(ctx, tenantID, res.WalletID, res.ID.String())
	if err != wallet.ErrNotFound {
		t.Errorf("open reservation lookup: got %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// ListOpenReservations — used by the F37 reconciler.
// ---------------------------------------------------------------------------

func TestListOpenReservations_OnlyUnsettled(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 100)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	svc, _ := usecase.NewService(repo, nil)

	// Two reservations, commit the first, leave the second open.
	r1, _ := svc.Reserve(ctx, tenantID, 10, "r1")
	if err := svc.Commit(ctx, r1, 10, "c1"); err != nil {
		t.Fatalf("Commit r1: %v", err)
	}
	r2, _ := svc.Reserve(ctx, tenantID, 20, "r2")

	open, err := repo.ListOpenReservations(ctx, tenantID, r2.WalletID)
	if err != nil {
		t.Fatalf("ListOpenReservations: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("open list = %d entries, want 1", len(open))
	}
	if open[0].ExternalRef != r2.ID.String() {
		t.Errorf("open list externalRef = %s, want %s", open[0].ExternalRef, r2.ID.String())
	}
}

// ---------------------------------------------------------------------------
// Lookup* arg validation.
// ---------------------------------------------------------------------------

func TestLookups_RejectZeroArgs(t *testing.T) {
	t.Parallel()
	db, _, _ := freshDBWithWalletTrigger(t)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	ctx := newWalletCtx(t)
	if _, err := repo.LookupByIdempotencyKey(ctx, uuid.Nil, uuid.New(), "k"); err != wallet.ErrNotFound {
		t.Errorf("LookupByIdempotencyKey(zero tenant): got %v", err)
	}
	if _, err := repo.LookupByIdempotencyKey(ctx, uuid.New(), uuid.New(), ""); err != wallet.ErrNotFound {
		t.Errorf("LookupByIdempotencyKey(empty key): got %v", err)
	}
	if _, err := repo.LookupCompletedByExternalRef(ctx, uuid.Nil, uuid.New(), "x"); err != wallet.ErrNotFound {
		t.Errorf("LookupCompleted(zero tenant): got %v", err)
	}
	if open, err := repo.ListOpenReservations(ctx, uuid.Nil, uuid.New()); err != nil || open != nil {
		t.Errorf("ListOpenReservations(zero tenant): %v / %v", err, open)
	}
}

// ---------------------------------------------------------------------------
// ApplyWithLock — targeted branch coverage. NotFound when no wallet
// row, IdempotencyConflict on duplicate ledger key, VersionConflict
// when the persisted version doesn't match the in-memory aggregate,
// the wallet=nil guard, and the wider nullIfEmpty path.
// ---------------------------------------------------------------------------

func TestApplyWithLock_NilWallet(t *testing.T) {
	t.Parallel()
	db, _, _ := freshDBWithWalletTrigger(t)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	if err := repo.ApplyWithLock(newWalletCtx(t), nil, nil); err == nil {
		t.Fatal("ApplyWithLock(nil): want error, got nil")
	}
}

func TestApplyWithLock_NotFound(t *testing.T) {
	t.Parallel()
	db, tenantID, _ := freshDBWithWalletTrigger(t)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	// Forge a wallet that doesn't exist in the DB.
	now := time.Now()
	ghost := wallet.NewHydrator().Hydrate(uuid.New(), tenantID, 0, 0, 1, now, now)
	if err := repo.ApplyWithLock(newWalletCtx(t), ghost, nil); !errors.Is(err, wallet.ErrNotFound) {
		t.Fatalf("ApplyWithLock(ghost): got %v, want ErrNotFound", err)
	}
}

func TestApplyWithLock_VersionConflict(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 100)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())

	// Load the real row, then forge a wallet at a wrong version.
	w, err := repo.LoadByTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("LoadByTenant: %v", err)
	}
	// Persisted version is 0; we send version=5 (delta != 1).
	bogus := wallet.NewHydrator().Hydrate(w.ID(), w.TenantID(), w.Balance(), w.Reserved(), 5, w.CreatedAt(), w.UpdatedAt())
	if err := repo.ApplyWithLock(ctx, bogus, nil); !errors.Is(err, wallet.ErrVersionConflict) {
		t.Fatalf("ApplyWithLock(version=5 vs persisted=0): got %v, want ErrVersionConflict", err)
	}
}

func TestApplyWithLock_IdempotencyConflict(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 100)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())

	w, _ := repo.LoadByTenant(ctx, tenantID)
	now := time.Now()
	if err := w.Reserve(10, now); err != nil {
		t.Fatalf("Reserve in memory: %v", err)
	}
	rid := uuid.New()
	entry := wallet.LedgerEntry{
		ID:             uuid.New(),
		WalletID:       w.ID(),
		TenantID:       w.TenantID(),
		Kind:           wallet.KindReserve,
		Amount:         wallet.SignedAmount(wallet.KindReserve, 10),
		IdempotencyKey: "dup",
		ExternalRef:    rid.String(),
		OccurredAt:     now,
		CreatedAt:      now,
	}
	if err := repo.ApplyWithLock(ctx, w, []wallet.LedgerEntry{entry}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	// Re-load and try to insert the same idempotency key. Reload so
	// the version stamp matches the DB.
	w2, _ := repo.LoadByTenant(ctx, tenantID)
	if err := w2.Reserve(5, now); err != nil {
		t.Fatalf("Reserve in memory (second): %v", err)
	}
	entry2 := wallet.LedgerEntry{
		ID:             uuid.New(),
		WalletID:       w2.ID(),
		TenantID:       w2.TenantID(),
		Kind:           wallet.KindReserve,
		Amount:         wallet.SignedAmount(wallet.KindReserve, 5),
		IdempotencyKey: "dup", // same key, different amount → still 23505 from the UNIQUE index
		ExternalRef:    uuid.New().String(),
		OccurredAt:     now,
		CreatedAt:      now,
	}
	if err := repo.ApplyWithLock(ctx, w2, []wallet.LedgerEntry{entry2}); !errors.Is(err, wallet.ErrIdempotencyConflict) {
		t.Fatalf("duplicate idempotency key: got %v, want ErrIdempotencyConflict", err)
	}
}

func TestApplyWithLock_EmptyExternalRef(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 0)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())

	w, _ := repo.LoadByTenant(ctx, tenantID)
	now := time.Now()
	if err := w.Grant(50, now); err != nil {
		t.Fatalf("Grant in memory: %v", err)
	}
	entry := wallet.LedgerEntry{
		ID:             uuid.New(),
		WalletID:       w.ID(),
		TenantID:       w.TenantID(),
		Kind:           wallet.KindGrant,
		Amount:         wallet.SignedAmount(wallet.KindGrant, 50),
		IdempotencyKey: "grant-1",
		ExternalRef:    "", // empty — nullIfEmpty should send NULL
		OccurredAt:     now,
		CreatedAt:      now,
	}
	if err := repo.ApplyWithLock(ctx, w, []wallet.LedgerEntry{entry}); err != nil {
		t.Fatalf("ApplyWithLock with empty external_ref: %v", err)
	}

	// Confirm the DB row has NULL external_ref.
	var hasRef bool
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT external_ref IS NOT NULL FROM token_ledger WHERE idempotency_key = $1`, "grant-1",
	).Scan(&hasRef); err != nil {
		t.Fatalf("read external_ref: %v", err)
	}
	if hasRef {
		t.Error("external_ref was not NULL after empty-string send")
	}
}

func TestApplyWithLock_LedgerInsertNonUniqueError(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 100)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())

	w, _ := repo.LoadByTenant(ctx, tenantID)
	now := time.Now()
	if err := w.Reserve(10, now); err != nil {
		t.Fatalf("Reserve in memory: %v", err)
	}
	// Use an invalid kind that the table CHECK constraint (added in
	// migration 0089) rejects. This forces a non-23505 error path.
	entry := wallet.LedgerEntry{
		ID:             uuid.New(),
		WalletID:       w.ID(),
		TenantID:       w.TenantID(),
		Kind:           wallet.LedgerKind("bogus"),
		Amount:         -10,
		IdempotencyKey: "bk",
		ExternalRef:    uuid.New().String(),
		OccurredAt:     now,
		CreatedAt:      now,
	}
	err := repo.ApplyWithLock(ctx, w, []wallet.LedgerEntry{entry})
	if err == nil {
		t.Fatal("ApplyWithLock with check-violating kind: want error, got nil")
	}
	if errors.Is(err, wallet.ErrIdempotencyConflict) || errors.Is(err, wallet.ErrVersionConflict) {
		t.Errorf("check violation surfaced as a known sentinel: %v", err)
	}
}

func TestListOpenReservations_ZeroWalletReturnsNil(t *testing.T) {
	t.Parallel()
	db, tenantID, _ := freshDBWithWalletTrigger(t)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	got, err := repo.ListOpenReservations(newWalletCtx(t), tenantID, uuid.Nil)
	if err != nil || got != nil {
		t.Errorf("ListOpenReservations(zero wallet): got %v / %v, want nil/nil", got, err)
	}
}
