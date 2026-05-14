package usecase_test

// Targeted tests for the error-handling branches that the happy-path
// and race tests don't reach: LookupBy* returning a non-NotFound
// error, ApplyWithLock surfacing a non-retriable error, the
// retry-exhaustion path, malformed external_ref on a retried reserve,
// idempotency-key reused with a wrong kind/extref/amount on the
// commit/release path, etc.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/wallet/usecase"
)

var errInjected = errors.New("injected adapter error")

func mkSvc(t *testing.T, repo wallet.Repository) *usecase.Service {
	t.Helper()
	svc, err := usecase.NewService(repo, func() time.Time { return fixedTime })
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// ---------------------------------------------------------------------------
// Reserve branches
// ---------------------------------------------------------------------------

func TestReserve_LookupReturnsUnexpectedError(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	tid := uuid.New()
	inner.seed(tid, 100, fixedTime)
	repo := newBuggyRepo(inner)
	repo.lookupByIdempotencyKey = func(context.Context, uuid.UUID, uuid.UUID, string) (wallet.LedgerEntry, error) {
		return wallet.LedgerEntry{}, errInjected
	}
	_, err := mkSvc(t, repo).Reserve(context.Background(), tid, 10, "op")
	if !errors.Is(err, errInjected) {
		t.Fatalf("Reserve with broken lookup: got %v, want errInjected", err)
	}
}

func TestReserve_LoadReturnsUnexpectedError(t *testing.T) {
	t.Parallel()
	repo := newBuggyRepo(newFakeRepo())
	repo.loadByTenant = func(context.Context, uuid.UUID) (*wallet.TokenWallet, error) {
		return nil, errInjected
	}
	_, err := mkSvc(t, repo).Reserve(context.Background(), uuid.New(), 1, "k")
	if !errors.Is(err, errInjected) {
		t.Fatalf("Reserve with broken load: got %v, want errInjected", err)
	}
}

func TestReserve_ApplyReturnsUnexpectedError(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	tid := uuid.New()
	inner.seed(tid, 100, fixedTime)
	repo := newBuggyRepo(inner)
	repo.applyWithLock = func(context.Context, *wallet.TokenWallet, []wallet.LedgerEntry) error {
		return errInjected
	}
	_, err := mkSvc(t, repo).Reserve(context.Background(), tid, 10, "op")
	if !errors.Is(err, errInjected) {
		t.Fatalf("Reserve with broken apply: got %v, want errInjected", err)
	}
}

func TestReserve_ExhaustedRetries(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	tid := uuid.New()
	inner.seed(tid, 100, fixedTime)
	repo := newBuggyRepo(inner)
	repo.applyWithLock = func(context.Context, *wallet.TokenWallet, []wallet.LedgerEntry) error {
		return wallet.ErrVersionConflict
	}
	_, err := mkSvc(t, repo).Reserve(context.Background(), tid, 10, "op")
	if err == nil {
		t.Fatal("Reserve with always-conflict apply: want error, got nil")
	}
	if !errors.Is(err, wallet.ErrVersionConflict) {
		t.Errorf("error chain missing ErrVersionConflict: %v", err)
	}
}

func TestReserve_RetryIdempotencyConflictThenSucceed(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	tid := uuid.New()
	inner.seed(tid, 100, fixedTime)
	repo := newBuggyRepo(inner)

	// First Apply returns idempotency conflict (a peer landed the
	// same reserve key between our Lookup and Apply). On retry, the
	// Lookup finds the prior row and returns its reservation.
	first := true
	priorReservationID := uuid.New()
	repo.applyWithLock = func(ctx context.Context, w *wallet.TokenWallet, entries []wallet.LedgerEntry) error {
		if first {
			first = false
			// Simulate the peer's row appearing in the ledger after
			// our lookup but before our apply.
			inner.mu.Lock()
			inner.ledger = append(inner.ledger, wallet.LedgerEntry{
				WalletID: entries[0].WalletID, TenantID: tid,
				Kind:           wallet.KindReserve,
				Amount:         -10,
				IdempotencyKey: "op",
				ExternalRef:    priorReservationID.String(),
				OccurredAt:     fixedTime,
			})
			inner.mu.Unlock()
			return wallet.ErrIdempotencyConflict
		}
		return inner.ApplyWithLock(ctx, w, entries)
	}
	res, err := mkSvc(t, repo).Reserve(context.Background(), tid, 10, "op")
	if err != nil {
		t.Fatalf("Reserve after idempotency-conflict-then-retry: %v", err)
	}
	if res.ID != priorReservationID {
		t.Errorf("retried reserve returned its own row instead of the peer's: %s vs %s", res.ID, priorReservationID)
	}
}

func TestReserve_PriorRowIsDifferentKind(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	tid := uuid.New()
	wid := inner.seed(tid, 100, fixedTime)
	// A grant row with the same idempotency key.
	inner.ledger = append(inner.ledger, wallet.LedgerEntry{
		WalletID: wid, TenantID: tid, Kind: wallet.KindGrant, Amount: 5, IdempotencyKey: "key", OccurredAt: fixedTime,
	})
	_, err := mkSvc(t, inner).Reserve(context.Background(), tid, 5, "key")
	if !errors.Is(err, wallet.ErrIdempotencyConflict) {
		t.Fatalf("Reserve with same key as grant: got %v, want ErrIdempotencyConflict", err)
	}
}

func TestReserve_PriorRowMalformedExternalRef(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	tid := uuid.New()
	wid := inner.seed(tid, 100, fixedTime)
	inner.ledger = append(inner.ledger, wallet.LedgerEntry{
		WalletID: wid, TenantID: tid, Kind: wallet.KindReserve, Amount: -5,
		IdempotencyKey: "key", ExternalRef: "not-a-uuid", OccurredAt: fixedTime,
	})
	_, err := mkSvc(t, inner).Reserve(context.Background(), tid, 5, "key")
	if err == nil {
		t.Fatal("Reserve with malformed ExternalRef: want error, got nil")
	}
}

// ---------------------------------------------------------------------------
// Commit branches
// ---------------------------------------------------------------------------

func setupReservation(t *testing.T, repo *fakeRepo) (*wallet.Reservation, *usecase.Service, uuid.UUID) {
	t.Helper()
	tid := uuid.New()
	repo.seed(tid, 100, fixedTime)
	svc := mkSvc(t, repo)
	res, err := svc.Reserve(context.Background(), tid, 50, "rsv")
	if err != nil {
		t.Fatalf("setup Reserve: %v", err)
	}
	return res, svc, tid
}

func TestCommit_IdempotencyKey_PriorIsDifferentKind(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, tid := setupReservation(t, inner)
	// Seed a grant row with the same key we'll use for commit.
	inner.ledger = append(inner.ledger, wallet.LedgerEntry{
		WalletID: res.WalletID, TenantID: tid, Kind: wallet.KindGrant,
		Amount: 5, IdempotencyKey: "cmt-key", OccurredAt: fixedTime,
	})
	err := mkSvc(t, inner).Commit(context.Background(), res, 10, "cmt-key")
	if !errors.Is(err, wallet.ErrIdempotencyConflict) {
		t.Fatalf("Commit with key on grant row: got %v, want ErrIdempotencyConflict", err)
	}
}

func TestCommit_IdempotencyKey_PriorWrongExternalRef(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, tid := setupReservation(t, inner)
	// Seed a commit row with same key but different externalRef.
	inner.ledger = append(inner.ledger, wallet.LedgerEntry{
		WalletID: res.WalletID, TenantID: tid, Kind: wallet.KindCommit, Amount: -10,
		IdempotencyKey: "cmt-key", ExternalRef: uuid.New().String(), OccurredAt: fixedTime,
	})
	err := mkSvc(t, inner).Commit(context.Background(), res, 10, "cmt-key")
	if !errors.Is(err, wallet.ErrIdempotencyConflict) {
		t.Fatalf("Commit with prior different externalRef: got %v, want ErrIdempotencyConflict", err)
	}
}

func TestCommit_IdempotencyKey_PriorWrongAmount(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, tid := setupReservation(t, inner)
	inner.ledger = append(inner.ledger, wallet.LedgerEntry{
		WalletID: res.WalletID, TenantID: tid, Kind: wallet.KindCommit, Amount: -20,
		IdempotencyKey: "cmt-key", ExternalRef: res.ID.String(), OccurredAt: fixedTime,
	})
	err := mkSvc(t, inner).Commit(context.Background(), res, 10, "cmt-key")
	if !errors.Is(err, wallet.ErrIdempotencyConflict) {
		t.Fatalf("Commit with prior different amount: got %v, want ErrIdempotencyConflict", err)
	}
}

func TestCommit_LoadReturnsUnexpectedError(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, _ := setupReservation(t, inner)
	repo := newBuggyRepo(inner)
	repo.loadByTenant = func(context.Context, uuid.UUID) (*wallet.TokenWallet, error) {
		return nil, errInjected
	}
	err := mkSvc(t, repo).Commit(context.Background(), res, 10, "k")
	if !errors.Is(err, errInjected) {
		t.Fatalf("Commit with broken load: got %v, want errInjected", err)
	}
}

func TestCommit_LookupIdemReturnsUnexpectedError(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, _ := setupReservation(t, inner)
	repo := newBuggyRepo(inner)
	repo.lookupByIdempotencyKey = func(context.Context, uuid.UUID, uuid.UUID, string) (wallet.LedgerEntry, error) {
		return wallet.LedgerEntry{}, errInjected
	}
	err := mkSvc(t, repo).Commit(context.Background(), res, 10, "k")
	if !errors.Is(err, errInjected) {
		t.Fatalf("Commit broken idem lookup: got %v, want errInjected", err)
	}
}

func TestCommit_LookupCompletedReturnsUnexpectedError(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, _ := setupReservation(t, inner)
	repo := newBuggyRepo(inner)
	repo.lookupCompletedByExternalRef = func(context.Context, uuid.UUID, uuid.UUID, string) (wallet.LedgerEntry, error) {
		return wallet.LedgerEntry{}, errInjected
	}
	err := mkSvc(t, repo).Commit(context.Background(), res, 10, "k")
	if !errors.Is(err, errInjected) {
		t.Fatalf("Commit broken completed lookup: got %v, want errInjected", err)
	}
}

func TestCommit_ApplyReturnsUnexpectedError(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, _ := setupReservation(t, inner)
	repo := newBuggyRepo(inner)
	repo.applyWithLock = func(context.Context, *wallet.TokenWallet, []wallet.LedgerEntry) error {
		return errInjected
	}
	err := mkSvc(t, repo).Commit(context.Background(), res, 10, "k")
	if !errors.Is(err, errInjected) {
		t.Fatalf("Commit broken apply: got %v, want errInjected", err)
	}
}

func TestCommit_ExhaustedRetries(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, _ := setupReservation(t, inner)
	repo := newBuggyRepo(inner)
	repo.applyWithLock = func(context.Context, *wallet.TokenWallet, []wallet.LedgerEntry) error {
		return wallet.ErrVersionConflict
	}
	err := mkSvc(t, repo).Commit(context.Background(), res, 10, "k")
	if !errors.Is(err, wallet.ErrVersionConflict) {
		t.Errorf("Commit exhausted retries: got %v, want chain containing ErrVersionConflict", err)
	}
}

func TestCommit_DomainErrorBubblesUp(t *testing.T) {
	t.Parallel()
	// res.Amount > w.reserved triggers ErrVersionConflict from
	// w.Commit (not from ApplyWithLock). The use-case must retry,
	// not surface ErrInvalidAmount or hang.
	inner := newFakeRepo()
	res, _, _ := setupReservation(t, inner)
	// Forcibly drop the wallet's reserved to zero so the domain's
	// version-conflict path fires.
	inner.mu.Lock()
	inner.wallets[res.WalletID].reserved = 0
	inner.mu.Unlock()
	err := mkSvc(t, inner).Commit(context.Background(), res, res.Amount, "k")
	if !errors.Is(err, wallet.ErrVersionConflict) {
		t.Errorf("expected ErrVersionConflict from domain, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Release branches
// ---------------------------------------------------------------------------

func TestRelease_PriorIsDifferentKind(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, tid := setupReservation(t, inner)
	inner.ledger = append(inner.ledger, wallet.LedgerEntry{
		WalletID: res.WalletID, TenantID: tid, Kind: wallet.KindCommit,
		Amount: -5, IdempotencyKey: "rel-k", ExternalRef: res.ID.String(), OccurredAt: fixedTime,
	})
	err := mkSvc(t, inner).Release(context.Background(), res, "rel-k")
	if !errors.Is(err, wallet.ErrIdempotencyConflict) {
		t.Fatalf("Release prior commit-row: got %v, want ErrIdempotencyConflict", err)
	}
}

func TestRelease_PriorWrongExternalRef(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, tid := setupReservation(t, inner)
	inner.ledger = append(inner.ledger, wallet.LedgerEntry{
		WalletID: res.WalletID, TenantID: tid, Kind: wallet.KindRelease,
		Amount: 5, IdempotencyKey: "rel-k", ExternalRef: uuid.New().String(), OccurredAt: fixedTime,
	})
	err := mkSvc(t, inner).Release(context.Background(), res, "rel-k")
	if !errors.Is(err, wallet.ErrIdempotencyConflict) {
		t.Fatalf("Release prior different extref: got %v, want ErrIdempotencyConflict", err)
	}
}

func TestRelease_PriorWrongAmount(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, tid := setupReservation(t, inner)
	inner.ledger = append(inner.ledger, wallet.LedgerEntry{
		WalletID: res.WalletID, TenantID: tid, Kind: wallet.KindRelease,
		Amount: 1, IdempotencyKey: "rel-k", ExternalRef: res.ID.String(), OccurredAt: fixedTime,
	})
	err := mkSvc(t, inner).Release(context.Background(), res, "rel-k")
	if !errors.Is(err, wallet.ErrIdempotencyConflict) {
		t.Fatalf("Release prior wrong amount: got %v, want ErrIdempotencyConflict", err)
	}
}

func TestRelease_LookupIdemReturnsUnexpectedError(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, _ := setupReservation(t, inner)
	repo := newBuggyRepo(inner)
	repo.lookupByIdempotencyKey = func(context.Context, uuid.UUID, uuid.UUID, string) (wallet.LedgerEntry, error) {
		return wallet.LedgerEntry{}, errInjected
	}
	err := mkSvc(t, repo).Release(context.Background(), res, "k")
	if !errors.Is(err, errInjected) {
		t.Fatalf("Release broken idem lookup: got %v, want errInjected", err)
	}
}

func TestRelease_LookupCompletedReturnsUnexpectedError(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, _ := setupReservation(t, inner)
	repo := newBuggyRepo(inner)
	repo.lookupCompletedByExternalRef = func(context.Context, uuid.UUID, uuid.UUID, string) (wallet.LedgerEntry, error) {
		return wallet.LedgerEntry{}, errInjected
	}
	err := mkSvc(t, repo).Release(context.Background(), res, "k")
	if !errors.Is(err, errInjected) {
		t.Fatalf("Release broken completed lookup: got %v, want errInjected", err)
	}
}

func TestRelease_LoadReturnsUnexpectedError(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, _ := setupReservation(t, inner)
	repo := newBuggyRepo(inner)
	repo.loadByTenant = func(context.Context, uuid.UUID) (*wallet.TokenWallet, error) {
		return nil, errInjected
	}
	err := mkSvc(t, repo).Release(context.Background(), res, "k")
	if !errors.Is(err, errInjected) {
		t.Fatalf("Release broken load: got %v, want errInjected", err)
	}
}

func TestRelease_ApplyReturnsUnexpectedError(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, _ := setupReservation(t, inner)
	repo := newBuggyRepo(inner)
	repo.applyWithLock = func(context.Context, *wallet.TokenWallet, []wallet.LedgerEntry) error {
		return errInjected
	}
	err := mkSvc(t, repo).Release(context.Background(), res, "k")
	if !errors.Is(err, errInjected) {
		t.Fatalf("Release broken apply: got %v, want errInjected", err)
	}
}

func TestRelease_ExhaustedRetries(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, _ := setupReservation(t, inner)
	repo := newBuggyRepo(inner)
	repo.applyWithLock = func(context.Context, *wallet.TokenWallet, []wallet.LedgerEntry) error {
		return wallet.ErrVersionConflict
	}
	err := mkSvc(t, repo).Release(context.Background(), res, "k")
	if !errors.Is(err, wallet.ErrVersionConflict) {
		t.Errorf("Release exhausted retries: got %v, want ErrVersionConflict", err)
	}
}

func TestRelease_DomainErrorBubblesUp(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	res, _, _ := setupReservation(t, inner)
	inner.mu.Lock()
	inner.wallets[res.WalletID].reserved = 0
	inner.mu.Unlock()
	err := mkSvc(t, inner).Release(context.Background(), res, "k")
	if !errors.Is(err, wallet.ErrVersionConflict) {
		t.Errorf("Release domain version conflict: got %v", err)
	}
}

func TestRelease_StaleWallet(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	tid := uuid.New()
	inner.seed(tid, 100, fixedTime)
	stale := &wallet.Reservation{ID: uuid.New(), TenantID: tid, WalletID: uuid.New(), Amount: 5}
	err := mkSvc(t, inner).Release(context.Background(), stale, "k")
	if !errors.Is(err, wallet.ErrNotFound) {
		t.Fatalf("stale wallet release: got %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Grant branches
// ---------------------------------------------------------------------------

func TestGrant_PriorIsDifferentKind(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	tid := uuid.New()
	wid := inner.seed(tid, 0, fixedTime)
	inner.ledger = append(inner.ledger, wallet.LedgerEntry{
		WalletID: wid, TenantID: tid, Kind: wallet.KindReserve,
		Amount: -5, IdempotencyKey: "g-k", OccurredAt: fixedTime,
	})
	err := mkSvc(t, inner).Grant(context.Background(), tid, 5, "g-k", "src")
	if !errors.Is(err, wallet.ErrIdempotencyConflict) {
		t.Fatalf("Grant key collides with reserve: got %v, want ErrIdempotencyConflict", err)
	}
}

func TestGrant_PriorWrongAmount(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	tid := uuid.New()
	wid := inner.seed(tid, 0, fixedTime)
	inner.ledger = append(inner.ledger, wallet.LedgerEntry{
		WalletID: wid, TenantID: tid, Kind: wallet.KindGrant,
		Amount: 100, IdempotencyKey: "g-k", OccurredAt: fixedTime,
	})
	err := mkSvc(t, inner).Grant(context.Background(), tid, 200, "g-k", "src")
	if !errors.Is(err, wallet.ErrIdempotencyConflict) {
		t.Fatalf("Grant key with different amount: got %v, want ErrIdempotencyConflict", err)
	}
}

func TestGrant_LoadReturnsUnexpectedError(t *testing.T) {
	t.Parallel()
	repo := newBuggyRepo(newFakeRepo())
	repo.loadByTenant = func(context.Context, uuid.UUID) (*wallet.TokenWallet, error) {
		return nil, errInjected
	}
	err := mkSvc(t, repo).Grant(context.Background(), uuid.New(), 1, "k", "src")
	if !errors.Is(err, errInjected) {
		t.Fatalf("Grant broken load: got %v, want errInjected", err)
	}
}

func TestGrant_LookupReturnsUnexpectedError(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	tid := uuid.New()
	inner.seed(tid, 0, fixedTime)
	repo := newBuggyRepo(inner)
	repo.lookupByIdempotencyKey = func(context.Context, uuid.UUID, uuid.UUID, string) (wallet.LedgerEntry, error) {
		return wallet.LedgerEntry{}, errInjected
	}
	err := mkSvc(t, repo).Grant(context.Background(), tid, 1, "k", "src")
	if !errors.Is(err, errInjected) {
		t.Fatalf("Grant broken idem lookup: got %v, want errInjected", err)
	}
}

func TestGrant_ApplyReturnsUnexpectedError(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	tid := uuid.New()
	inner.seed(tid, 0, fixedTime)
	repo := newBuggyRepo(inner)
	repo.applyWithLock = func(context.Context, *wallet.TokenWallet, []wallet.LedgerEntry) error {
		return errInjected
	}
	err := mkSvc(t, repo).Grant(context.Background(), tid, 1, "k", "src")
	if !errors.Is(err, errInjected) {
		t.Fatalf("Grant broken apply: got %v, want errInjected", err)
	}
}

func TestGrant_ExhaustedRetries(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	tid := uuid.New()
	inner.seed(tid, 0, fixedTime)
	repo := newBuggyRepo(inner)
	repo.applyWithLock = func(context.Context, *wallet.TokenWallet, []wallet.LedgerEntry) error {
		return wallet.ErrVersionConflict
	}
	err := mkSvc(t, repo).Grant(context.Background(), tid, 1, "k", "src")
	if !errors.Is(err, wallet.ErrVersionConflict) {
		t.Errorf("Grant exhausted retries: got %v, want chain containing ErrVersionConflict", err)
	}
}

// ---------------------------------------------------------------------------
// Coverage of edge cases on the LookupByIdempotencyKey reserve retry
// when the prior entry came in with a different amount.
// ---------------------------------------------------------------------------

func TestReserve_PriorRowDifferentAmount(t *testing.T) {
	t.Parallel()
	inner := newFakeRepo()
	tid := uuid.New()
	wid := inner.seed(tid, 100, fixedTime)
	inner.ledger = append(inner.ledger, wallet.LedgerEntry{
		WalletID: wid, TenantID: tid, Kind: wallet.KindReserve,
		Amount: -7, IdempotencyKey: "k", ExternalRef: uuid.New().String(), OccurredAt: fixedTime,
	})
	_, err := mkSvc(t, inner).Reserve(context.Background(), tid, 5, "k")
	if !errors.Is(err, wallet.ErrIdempotencyConflict) {
		t.Fatalf("Reserve key with different amount: got %v, want ErrIdempotencyConflict", err)
	}
}

// suppress unused warnings on helper imports
var _ = fmt.Sprintf
