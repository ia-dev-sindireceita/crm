package postgres_test

// SIN-62730 integration tests for the courtesy-grant onboarding
// adapter. Like wallet_adapter_test.go (SIN-62750 / SIN-62726), the
// tests live in the parent postgres_test package — not in the
// internal/adapter/db/postgres/wallet subpackage — to avoid the
// shared-cluster ALTER ROLE race that two parallel test binaries
// would otherwise trigger on a fresh testpg.Start().
//
// The migrations needed (0004 tenants, 0005 users, 0089 wallet/grant
// /ledger, 0090 wallet updated_at trigger) are applied by the shared
// freshDBWithWalletTrigger helper in wallet_adapter_test.go, so the
// fixture surface stays a single call.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	walletadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres/wallet"
	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/wallet/usecase"
)

const courtesyAmount int64 = 10_000

// ---------------------------------------------------------------------------
// Constructor — nil pool is rejected
// ---------------------------------------------------------------------------

func TestNewCourtesyStore_RejectsNilPool(t *testing.T) {
	t.Parallel()
	if _, err := walletadapter.NewCourtesyStore(nil); err == nil {
		t.Fatal("NewCourtesyStore(nil): want error, got nil")
	}
}

// ---------------------------------------------------------------------------
// Issue happy path — first call creates wallet + grant + ledger row,
// balance materializes to the configured amount, version=1.
// ---------------------------------------------------------------------------

func TestCourtesyStore_Issue_HappyPath(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	store, err := walletadapter.NewCourtesyStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewCourtesyStore: %v", err)
	}

	out, err := store.Issue(ctx, tenantID, masterID, courtesyAmount)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !out.Granted {
		t.Errorf("Granted = false, want true on first issue")
	}
	if out.WalletID == uuid.Nil || out.GrantID == uuid.Nil {
		t.Errorf("missing IDs: %+v", out)
	}

	// Verify wallet row materialized at balance=amount, version=1.
	var balance, reserved, version int64
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT balance, reserved, version FROM token_wallet WHERE id = $1`, out.WalletID,
	).Scan(&balance, &reserved, &version); err != nil {
		t.Fatalf("read wallet: %v", err)
	}
	if balance != courtesyAmount || reserved != 0 || version != 1 {
		t.Errorf("wallet state: bal=%d rsv=%d ver=%d, want %d/0/1", balance, reserved, version, courtesyAmount)
	}

	// Verify courtesy_grant row.
	var grantTenant uuid.UUID
	var grantAmount int64
	var grantedBy uuid.UUID
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT tenant_id, amount, granted_by_user_id FROM courtesy_grant WHERE id = $1`, out.GrantID,
	).Scan(&grantTenant, &grantAmount, &grantedBy); err != nil {
		t.Fatalf("read courtesy_grant: %v", err)
	}
	if grantTenant != tenantID || grantAmount != courtesyAmount || grantedBy != masterID {
		t.Errorf("grant row: tenant=%s amount=%d by=%s, want %s/%d/%s",
			grantTenant, grantAmount, grantedBy, tenantID, courtesyAmount, masterID)
	}

	// Verify ledger row: kind=grant, signed positive, idempotency_key='courtesy:<tenant>'.
	var (
		ledgerKind, idemKey, externalRef string
		ledgerAmount                     int64
		ledgerWalletID                   uuid.UUID
	)
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT wallet_id, kind, amount, idempotency_key, COALESCE(external_ref, '')
		   FROM token_ledger
		  WHERE wallet_id = $1 AND idempotency_key = $2`,
		out.WalletID, walletadapter.CourtesyIdempotencyKey(tenantID),
	).Scan(&ledgerWalletID, &ledgerKind, &ledgerAmount, &idemKey, &externalRef); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if ledgerKind != "grant" || ledgerAmount != courtesyAmount || ledgerWalletID != out.WalletID {
		t.Errorf("ledger row: kind=%s amount=%d wallet=%s, want grant/%d/%s",
			ledgerKind, ledgerAmount, ledgerWalletID, courtesyAmount, out.WalletID)
	}
	if externalRef != out.GrantID.String() {
		t.Errorf("ledger external_ref = %s, want grant id %s", externalRef, out.GrantID)
	}
	if idemKey != walletadapter.CourtesyIdempotencyKey(tenantID) {
		t.Errorf("ledger idempotency_key = %s, want %s", idemKey, walletadapter.CourtesyIdempotencyKey(tenantID))
	}
}

// ---------------------------------------------------------------------------
// Issue idempotency — second call is a silent no-op, IDs stay stable,
// the database state is unchanged (one ledger row, one grant row).
// ---------------------------------------------------------------------------

func TestCourtesyStore_Issue_NoOpOnDuplicate(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	store, err := walletadapter.NewCourtesyStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewCourtesyStore: %v", err)
	}

	first, err := store.Issue(ctx, tenantID, masterID, courtesyAmount)
	if err != nil {
		t.Fatalf("Issue#1: %v", err)
	}
	second, err := store.Issue(ctx, tenantID, masterID, courtesyAmount)
	if err != nil {
		t.Fatalf("Issue#2: %v", err)
	}
	if second.Granted {
		t.Error("Issue#2 Granted=true, want false")
	}
	if second.WalletID != first.WalletID || second.GrantID != first.GrantID {
		t.Errorf("Issue#2 IDs drifted: first=%+v second=%+v", first, second)
	}

	// Exactly one grant row and one ledger row.
	assertCounts(t, ctx, db, tenantID, first.WalletID, 1, 1)

	// Balance must still equal courtesyAmount, version=1.
	var balance, version int64
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT balance, version FROM token_wallet WHERE id = $1`, first.WalletID,
	).Scan(&balance, &version); err != nil {
		t.Fatalf("read wallet: %v", err)
	}
	if balance != courtesyAmount || version != 1 {
		t.Errorf("wallet state after retry: bal=%d ver=%d, want %d/1", balance, version, courtesyAmount)
	}
}

// ---------------------------------------------------------------------------
// Validation — Issue rejects zero tenant / zero actor / non-positive amount.
// ---------------------------------------------------------------------------

func TestCourtesyStore_Issue_RejectsZeroTenant(t *testing.T) {
	t.Parallel()
	db, _, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	store, _ := walletadapter.NewCourtesyStore(db.MasterOpsPool())
	if _, err := store.Issue(ctx, uuid.Nil, masterID, courtesyAmount); !errors.Is(err, wallet.ErrZeroTenant) {
		t.Fatalf("Issue(zero tenant): got %v, want ErrZeroTenant", err)
	}
}

func TestCourtesyStore_Issue_RejectsZeroActor(t *testing.T) {
	t.Parallel()
	db, tenantID, _ := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	store, _ := walletadapter.NewCourtesyStore(db.MasterOpsPool())
	if _, err := store.Issue(ctx, tenantID, uuid.Nil, courtesyAmount); !errors.Is(err, postgresadapter.ErrZeroActor) {
		t.Fatalf("Issue(zero actor): got %v, want ErrZeroActor", err)
	}
}

func TestCourtesyStore_Issue_RejectsNonPositiveAmount(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	store, _ := walletadapter.NewCourtesyStore(db.MasterOpsPool())
	for _, amt := range []int64{0, -1, -10_000} {
		if _, err := store.Issue(ctx, tenantID, masterID, amt); !errors.Is(err, wallet.ErrInvalidAmount) {
			t.Errorf("Issue(amount=%d): got %v, want ErrInvalidAmount", amt, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Issue + use-case service — disabled flag short-circuits before the DB.
// ---------------------------------------------------------------------------

func TestCourtesyStore_Disabled_Short_Circuits(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	store, _ := walletadapter.NewCourtesyStore(db.MasterOpsPool())
	svc, err := usecase.NewIssueCourtesyGrantService(store, usecase.IssueCourtesyGrantConfig{
		Amount:   courtesyAmount,
		Disabled: true,
		ActorID:  masterID,
	})
	if err != nil {
		t.Fatalf("NewIssueCourtesyGrantService: %v", err)
	}
	_, err = svc.Issue(ctx, tenantID)
	if !errors.Is(err, wallet.ErrCourtesyGrantDisabled) {
		t.Fatalf("Issue: got %v, want ErrCourtesyGrantDisabled", err)
	}
	// No grant row should have been written.
	assertCounts(t, ctx, db, tenantID, uuid.Nil, 0, 0)
}

// ---------------------------------------------------------------------------
// Race test — 50 goroutines call Issue for the same tenant. Exactly
// one wins (Granted=true), 49 see Granted=false with the same wallet
// id; database state shows 1 wallet + 1 grant + 1 ledger row.
// ---------------------------------------------------------------------------

func TestCourtesyStore_FiftyConcurrentIssues(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	store, err := walletadapter.NewCourtesyStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewCourtesyStore: %v", err)
	}

	const N = 50
	var (
		wins   atomic.Int64
		noOps  atomic.Int64
		errs   atomic.Int64
		errMsg atomic.Value
		seenW  atomic.Pointer[uuid.UUID]
		seenG  atomic.Pointer[uuid.UUID]

		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			out, err := store.Issue(ctx, tenantID, masterID, courtesyAmount)
			if err != nil {
				errs.Add(1)
				errMsg.Store(err.Error())
				return
			}
			if out.Granted {
				wins.Add(1)
			} else {
				noOps.Add(1)
			}
			// Every caller observes the same wallet + grant id.
			if prev := seenW.Load(); prev == nil {
				wid := out.WalletID
				seenW.CompareAndSwap(nil, &wid)
			} else if *prev != out.WalletID {
				t.Errorf("wallet id mismatch: prev=%s now=%s", *prev, out.WalletID)
			}
			if prev := seenG.Load(); prev == nil {
				gid := out.GrantID
				seenG.CompareAndSwap(nil, &gid)
			} else if *prev != out.GrantID {
				t.Errorf("grant id mismatch: prev=%s now=%s", *prev, out.GrantID)
			}
		}()
	}
	close(start)
	wg.Wait()

	if errs.Load() != 0 {
		t.Fatalf("got %d concurrent errors; first message: %v", errs.Load(), errMsg.Load())
	}
	if wins.Load() != 1 {
		t.Errorf("wins = %d, want exactly 1", wins.Load())
	}
	if noOps.Load() != N-1 {
		t.Errorf("noOps = %d, want %d", noOps.Load(), N-1)
	}

	walletPtr := seenW.Load()
	if walletPtr == nil {
		t.Fatal("seenW is nil — no caller observed a wallet id")
	}
	assertCounts(t, ctx, db, tenantID, *walletPtr, 1, 1)

	// Balance materializes once, version=1, no over-credit.
	var balance, version int64
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT balance, version FROM token_wallet WHERE id = $1`, *walletPtr,
	).Scan(&balance, &version); err != nil {
		t.Fatalf("read wallet: %v", err)
	}
	if balance != courtesyAmount || version != 1 {
		t.Errorf("post-race wallet state: bal=%d ver=%d, want %d/1", balance, version, courtesyAmount)
	}
}

// ---------------------------------------------------------------------------
// Defense in depth — even a manual second insert with the same
// idempotency_key on the same wallet is rejected by the UNIQUE index.
// ---------------------------------------------------------------------------

func TestCourtesyStore_LedgerUniqueRejectsDuplicateKey(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	store, _ := walletadapter.NewCourtesyStore(db.MasterOpsPool())
	out, err := store.Issue(ctx, tenantID, masterID, courtesyAmount)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Attempt a manual second insert with the same idempotency key —
	// the UNIQUE (wallet_id, idempotency_key) index must reject it.
	err = postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO token_ledger
			   (id, wallet_id, tenant_id, kind, amount, idempotency_key, external_ref, occurred_at, created_at)
			 VALUES (gen_random_uuid(), $1, $2, 'grant', 1, $3, NULL, now(), now())`,
			out.WalletID, tenantID, walletadapter.CourtesyIdempotencyKey(tenantID),
		)
		return err
	})
	if err == nil {
		t.Fatal("manual duplicate insert: want unique-violation error, got nil")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// assertCounts checks (grants, ledgerRows) for tenantID. walletID may
// be uuid.Nil when the caller hasn't observed a wallet (e.g. the
// disabled-flag test path); in that case the ledger count is checked
// across every row carrying the canonical idempotency_key for that
// tenant.
func assertCounts(t *testing.T, ctx context.Context, db *testpg.DB, tenantID, walletID uuid.UUID, wantGrants, wantLedger int) {
	t.Helper()
	var gotGrants int
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM courtesy_grant WHERE tenant_id = $1`, tenantID,
	).Scan(&gotGrants); err != nil {
		t.Fatalf("count courtesy_grant: %v", err)
	}
	if gotGrants != wantGrants {
		t.Errorf("courtesy_grant count for %s = %d, want %d", tenantID, gotGrants, wantGrants)
	}

	idem := walletadapter.CourtesyIdempotencyKey(tenantID)
	var gotLedger int
	if walletID == uuid.Nil {
		if err := db.SuperuserPool().QueryRow(ctx,
			`SELECT count(*) FROM token_ledger WHERE idempotency_key = $1`, idem,
		).Scan(&gotLedger); err != nil {
			t.Fatalf("count token_ledger: %v", err)
		}
	} else {
		if err := db.SuperuserPool().QueryRow(ctx,
			`SELECT count(*) FROM token_ledger WHERE wallet_id = $1 AND idempotency_key = $2`,
			walletID, idem,
		).Scan(&gotLedger); err != nil {
			t.Fatalf("count token_ledger: %v", err)
		}
	}
	if gotLedger != wantLedger {
		t.Errorf("token_ledger count for %s key=%s = %d, want %d", tenantID, idem, gotLedger, wantLedger)
	}
}
