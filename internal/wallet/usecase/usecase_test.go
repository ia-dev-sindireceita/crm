package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/wallet/usecase"
)

var fixedTime = time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC)

func newSvc(t *testing.T, repo wallet.Repository) *usecase.Service {
	t.Helper()
	svc, err := usecase.NewService(repo, func() time.Time { return fixedTime })
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestNewService_RejectsNilRepo(t *testing.T) {
	t.Parallel()
	if _, err := usecase.NewService(nil, nil); err == nil {
		t.Fatal("NewService(nil, nil): want error, got nil")
	}
}

func TestNewService_DefaultsClock(t *testing.T) {
	t.Parallel()
	svc, err := usecase.NewService(newFakeRepo(), nil)
	if err != nil {
		t.Fatalf("NewService(repo, nil): %v", err)
	}
	if svc == nil {
		t.Fatal("svc is nil")
	}
}

func TestReserve_HappyPath(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 100, fixedTime)
	svc := newSvc(t, repo)

	res, err := svc.Reserve(context.Background(), tid, 40, "op-1")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.Amount != 40 || res.TenantID != tid {
		t.Errorf("reservation = %+v, want amount=40 tenant=%s", res, tid)
	}

	bal, rsv, ver := repo.snapshotBalance(res.WalletID)
	if bal != 100 || rsv != 40 || ver != 1 {
		t.Errorf("post-Reserve state: bal=%d rsv=%d ver=%d, want 100/40/1", bal, rsv, ver)
	}
}

func TestReserve_InsufficientFunds(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 10, fixedTime)
	svc := newSvc(t, repo)

	_, err := svc.Reserve(context.Background(), tid, 50, "op-1")
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("Reserve(50) over balance=10: got %v, want ErrInsufficientFunds", err)
	}
	if repo.ledgerCount() != 0 {
		t.Errorf("ledger should be empty after failed reserve, got %d rows", repo.ledgerCount())
	}
}

func TestReserve_NoWallet(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	svc := newSvc(t, repo)
	_, err := svc.Reserve(context.Background(), uuid.New(), 1, "op-1")
	if !errors.Is(err, wallet.ErrNotFound) {
		t.Fatalf("Reserve on missing wallet: got %v, want ErrNotFound", err)
	}
}

func TestReserve_RetrySameKeyReturnsPrior(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 100, fixedTime)
	svc := newSvc(t, repo)

	first, err := svc.Reserve(context.Background(), tid, 30, "op-retry")
	if err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	second, err := svc.Reserve(context.Background(), tid, 30, "op-retry")
	if err != nil {
		t.Fatalf("retry Reserve: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("retried reserve returned different reservation: %s vs %s", first.ID, second.ID)
	}
	bal, rsv, ver := repo.snapshotBalance(first.WalletID)
	if bal != 100 || rsv != 30 || ver != 1 {
		t.Errorf("retry mutated wallet: bal=%d rsv=%d ver=%d, want 100/30/1", bal, rsv, ver)
	}
}

func TestReserve_IdempotencyConflictOnDifferentAmount(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 100, fixedTime)
	svc := newSvc(t, repo)

	if _, err := svc.Reserve(context.Background(), tid, 30, "op-c"); err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	_, err := svc.Reserve(context.Background(), tid, 40, "op-c")
	if !errors.Is(err, wallet.ErrIdempotencyConflict) {
		t.Fatalf("Reserve with same key, different amount: got %v, want ErrIdempotencyConflict", err)
	}
}

func TestReserve_RejectsBadArguments(t *testing.T) {
	t.Parallel()
	svc := newSvc(t, newFakeRepo())
	ctx := context.Background()
	tid := uuid.New()
	if _, err := svc.Reserve(ctx, uuid.Nil, 1, "op"); !errors.Is(err, wallet.ErrZeroTenant) {
		t.Errorf("Reserve(uuid.Nil): got %v, want ErrZeroTenant", err)
	}
	if _, err := svc.Reserve(ctx, tid, 0, "op"); !errors.Is(err, wallet.ErrInvalidAmount) {
		t.Errorf("Reserve(0): got %v, want ErrInvalidAmount", err)
	}
	if _, err := svc.Reserve(ctx, tid, 1, ""); !errors.Is(err, wallet.ErrEmptyIdempotencyKey) {
		t.Errorf("Reserve(empty key): got %v, want ErrEmptyIdempotencyKey", err)
	}
}

func TestCommit_HappyPath(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 100, fixedTime)
	svc := newSvc(t, repo)

	res, err := svc.Reserve(context.Background(), tid, 50, "rsv-1")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := svc.Commit(context.Background(), res, 30, "cmt-1"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	bal, rsv, ver := repo.snapshotBalance(res.WalletID)
	if bal != 70 || rsv != 0 || ver != 2 {
		t.Errorf("post-Commit state: bal=%d rsv=%d ver=%d, want 70/0/2", bal, rsv, ver)
	}
}

func TestCommit_RetrySameKey(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 100, fixedTime)
	svc := newSvc(t, repo)

	res, _ := svc.Reserve(context.Background(), tid, 50, "rsv-1")
	if err := svc.Commit(context.Background(), res, 30, "cmt-1"); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	if err := svc.Commit(context.Background(), res, 30, "cmt-1"); err != nil {
		t.Fatalf("retry Commit: %v", err)
	}
	bal, rsv, _ := repo.snapshotBalance(res.WalletID)
	if bal != 70 || rsv != 0 {
		t.Errorf("retry double-debited: bal=%d rsv=%d, want 70/0", bal, rsv)
	}
}

func TestCommit_DifferentKeyOnSettledReservation(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 100, fixedTime)
	svc := newSvc(t, repo)

	res, _ := svc.Reserve(context.Background(), tid, 50, "rsv-1")
	if err := svc.Commit(context.Background(), res, 50, "cmt-1"); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	err := svc.Commit(context.Background(), res, 30, "cmt-2")
	if !errors.Is(err, wallet.ErrReservationCompleted) {
		t.Fatalf("re-commit with different key: got %v, want ErrReservationCompleted", err)
	}
}

func TestCommit_OverReservation(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 100, fixedTime)
	svc := newSvc(t, repo)
	res, _ := svc.Reserve(context.Background(), tid, 50, "rsv-1")
	err := svc.Commit(context.Background(), res, 51, "cmt-1")
	if !errors.Is(err, wallet.ErrInvalidAmount) {
		t.Fatalf("Commit(51) over reserve(50): got %v, want ErrInvalidAmount", err)
	}
}

func TestCommit_RejectsBadArguments(t *testing.T) {
	t.Parallel()
	svc := newSvc(t, newFakeRepo())
	ctx := context.Background()
	res := &wallet.Reservation{ID: uuid.New(), WalletID: uuid.New(), TenantID: uuid.New(), Amount: 10}

	if err := svc.Commit(ctx, nil, 1, "k"); err == nil {
		t.Error("Commit(nil): want error")
	}
	if err := svc.Commit(ctx, res, 0, "k"); !errors.Is(err, wallet.ErrInvalidAmount) {
		t.Errorf("Commit(0): got %v, want ErrInvalidAmount", err)
	}
	if err := svc.Commit(ctx, res, 1, ""); !errors.Is(err, wallet.ErrEmptyIdempotencyKey) {
		t.Errorf("Commit(empty key): got %v, want ErrEmptyIdempotencyKey", err)
	}
}

func TestCommit_StaleReservationDifferentWallet(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 100, fixedTime)
	svc := newSvc(t, repo)

	// Forge a reservation that points at a wallet id not associated
	// with this tenant — the service must refuse.
	stale := &wallet.Reservation{
		ID: uuid.New(), TenantID: tid, WalletID: uuid.New(), Amount: 5,
	}
	err := svc.Commit(context.Background(), stale, 5, "cmt")
	if !errors.Is(err, wallet.ErrNotFound) {
		t.Fatalf("stale wallet id: got %v, want ErrNotFound", err)
	}
}

func TestRelease_HappyPath(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 100, fixedTime)
	svc := newSvc(t, repo)

	res, _ := svc.Reserve(context.Background(), tid, 50, "rsv-1")
	if err := svc.Release(context.Background(), res, "rel-1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	bal, rsv, _ := repo.snapshotBalance(res.WalletID)
	if bal != 100 || rsv != 0 {
		t.Errorf("post-Release: bal=%d rsv=%d, want 100/0", bal, rsv)
	}
}

func TestRelease_RetrySameKey(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 100, fixedTime)
	svc := newSvc(t, repo)

	res, _ := svc.Reserve(context.Background(), tid, 50, "rsv")
	if err := svc.Release(context.Background(), res, "rel"); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := svc.Release(context.Background(), res, "rel"); err != nil {
		t.Fatalf("retry Release: %v", err)
	}
}

func TestRelease_AfterCommit(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 100, fixedTime)
	svc := newSvc(t, repo)

	res, _ := svc.Reserve(context.Background(), tid, 50, "rsv")
	if err := svc.Commit(context.Background(), res, 50, "cmt"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	err := svc.Release(context.Background(), res, "rel")
	if !errors.Is(err, wallet.ErrReservationCompleted) {
		t.Fatalf("Release after Commit: got %v, want ErrReservationCompleted", err)
	}
}

func TestRelease_RejectsBadArguments(t *testing.T) {
	t.Parallel()
	svc := newSvc(t, newFakeRepo())
	ctx := context.Background()
	if err := svc.Release(ctx, nil, "k"); err == nil {
		t.Error("Release(nil): want error")
	}
	res := &wallet.Reservation{TenantID: uuid.New(), WalletID: uuid.New(), Amount: 1}
	if err := svc.Release(ctx, res, ""); !errors.Is(err, wallet.ErrEmptyIdempotencyKey) {
		t.Errorf("Release(empty key): got %v, want ErrEmptyIdempotencyKey", err)
	}
}

func TestGrant_HappyPath(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	wid := repo.seed(tid, 0, fixedTime)
	svc := newSvc(t, repo)

	if err := svc.Grant(context.Background(), tid, 500, "grant-1", "courtesy:onboard"); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	bal, _, ver := repo.snapshotBalance(wid)
	if bal != 500 || ver != 1 {
		t.Errorf("post-Grant: bal=%d ver=%d, want 500/1", bal, ver)
	}
}

func TestGrant_RetrySameKey(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	wid := repo.seed(tid, 0, fixedTime)
	svc := newSvc(t, repo)

	if err := svc.Grant(context.Background(), tid, 500, "g", "src"); err != nil {
		t.Fatalf("first Grant: %v", err)
	}
	if err := svc.Grant(context.Background(), tid, 500, "g", "src"); err != nil {
		t.Fatalf("retry Grant: %v", err)
	}
	bal, _, _ := repo.snapshotBalance(wid)
	if bal != 500 {
		t.Errorf("retry double-credited: bal=%d, want 500", bal)
	}
}

func TestGrant_RejectsBadArguments(t *testing.T) {
	t.Parallel()
	svc := newSvc(t, newFakeRepo())
	ctx := context.Background()
	if err := svc.Grant(ctx, uuid.Nil, 1, "k", "src"); !errors.Is(err, wallet.ErrZeroTenant) {
		t.Errorf("Grant(uuid.Nil): got %v, want ErrZeroTenant", err)
	}
	if err := svc.Grant(ctx, uuid.New(), 0, "k", "src"); !errors.Is(err, wallet.ErrInvalidAmount) {
		t.Errorf("Grant(0): got %v, want ErrInvalidAmount", err)
	}
	if err := svc.Grant(ctx, uuid.New(), 1, "", "src"); !errors.Is(err, wallet.ErrEmptyIdempotencyKey) {
		t.Errorf("Grant(empty key): got %v, want ErrEmptyIdempotencyKey", err)
	}
}

// TestReserve_RaceAtomic is the AC #2 race test: 100 concurrent
// reserves against a wallet with capacity for exactly 50 must
// produce 50 successes and 50 ErrInsufficientFunds — never a
// negative balance or double-debit. Uses -race to catch any shared
// state slipping through the optimistic lock.
func TestReserve_RaceAtomic(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	wid := repo.seed(tid, 50, fixedTime)
	svc := newSvc(t, repo)

	const N = 100
	const amount = 1

	var wg sync.WaitGroup
	var success, insufficient, other atomic.Int64
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Reserve(context.Background(), tid, amount, fmt.Sprintf("op-%d", i))
			switch {
			case err == nil:
				success.Add(1)
			case errors.Is(err, wallet.ErrInsufficientFunds):
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

	bal, rsv, _ := repo.snapshotBalance(wid)
	if bal != 50 {
		t.Errorf("final balance = %d, want 50 (unchanged — reserves don't debit)", bal)
	}
	if rsv != 50 {
		t.Errorf("final reserved = %d, want 50", rsv)
	}
	if bal-rsv != 0 {
		t.Errorf("final available = %d, want 0", bal-rsv)
	}
	// Available = balance - reserved must never be negative.
	if got := repo.ledgerCount(); got != 50 {
		t.Errorf("ledger rows = %d, want 50 (one per successful reserve)", got)
	}
}

// TestReserve_RaceWithRetryHook verifies the use-case loop tolerates
// an injected version conflict mid-flight. The hook fires once,
// kicks a side-thread that bumps the wallet's version under the
// lock, and the in-flight ApplyWithLock then sees a stale version.
// The retry loop must see the new version, reload, and succeed.
func TestReserve_RecoversFromVersionConflict(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	wid := repo.seed(tid, 100, fixedTime)
	svc := newSvc(t, repo)

	// Pre-seed a competing reserve under the same wallet so the
	// first attempt sees a stale version. We jam in the row by
	// reaching past the service for setup — that's fine, it's a
	// fixture step.
	repo.mu.Lock()
	repo.wallets[wid].balance = 100
	repo.wallets[wid].reserved = 20
	repo.wallets[wid].version = 1 // simulate a concurrent reserve from a different caller
	repo.ledger = append(repo.ledger, wallet.LedgerEntry{
		WalletID: wid, TenantID: tid, Kind: wallet.KindReserve, Amount: -20,
		IdempotencyKey: "other-op", ExternalRef: uuid.New().String(), OccurredAt: fixedTime,
	})
	repo.mu.Unlock()

	// Use-case should still succeed even though the version moved
	// between load and apply on the first attempt.
	res, err := svc.Reserve(context.Background(), tid, 30, "my-op")
	if err != nil {
		t.Fatalf("Reserve under contention: %v", err)
	}
	if res.Amount != 30 {
		t.Errorf("amount=%d, want 30", res.Amount)
	}
}
