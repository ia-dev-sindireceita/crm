package usecase_test

// SIN-62730 unit tests for the IssueCourtesyGrant flow. The fake
// CourtesyGrantRepository models the contract documented on
// wallet.CourtesyGrantRepository (atomic insert + UNIQUE on
// courtesy_grant.tenant_id) so the use-case retry/idempotency logic
// is exercised without touching Postgres. The integration suite in
// internal/adapter/db/postgres covers the real SQL semantics.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/wallet/usecase"
)

type fakeCourtesyRepo struct {
	mu      sync.Mutex
	granted map[uuid.UUID]wallet.Issued
	calls   atomic.Int64

	// failNext, when non-nil, is returned on the next Issue call (and
	// then cleared). Used to exercise the error path.
	failNext error
}

func newFakeCourtesyRepo() *fakeCourtesyRepo {
	return &fakeCourtesyRepo{granted: map[uuid.UUID]wallet.Issued{}}
}

func (r *fakeCourtesyRepo) Issue(ctx context.Context, tenantID, actorID uuid.UUID, amount int64) (wallet.Issued, error) {
	if err := ctx.Err(); err != nil {
		return wallet.Issued{}, err
	}
	r.calls.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext != nil {
		err := r.failNext
		r.failNext = nil
		return wallet.Issued{}, err
	}
	if existing, ok := r.granted[tenantID]; ok {
		return wallet.Issued{Granted: false, WalletID: existing.WalletID, GrantID: existing.GrantID}, nil
	}
	out := wallet.Issued{Granted: true, WalletID: uuid.New(), GrantID: uuid.New()}
	r.granted[tenantID] = out
	return out, nil
}

func validCfg(t *testing.T) usecase.IssueCourtesyGrantConfig {
	t.Helper()
	return usecase.IssueCourtesyGrantConfig{
		Amount:  10_000,
		ActorID: uuid.New(),
	}
}

// ---------------------------------------------------------------------------
// Constructor validation
// ---------------------------------------------------------------------------

func TestNewIssueCourtesyGrantService_RejectsNilRepo(t *testing.T) {
	t.Parallel()
	_, err := usecase.NewIssueCourtesyGrantService(nil, validCfg(t))
	if err == nil {
		t.Fatal("want error for nil repo, got nil")
	}
}

func TestNewIssueCourtesyGrantService_RejectsNonPositiveAmount(t *testing.T) {
	t.Parallel()
	for _, amt := range []int64{0, -1, -10_000} {
		cfg := validCfg(t)
		cfg.Amount = amt
		if _, err := usecase.NewIssueCourtesyGrantService(newFakeCourtesyRepo(), cfg); err == nil {
			t.Errorf("amount=%d: want error, got nil", amt)
		}
	}
}

func TestNewIssueCourtesyGrantService_RejectsZeroActor(t *testing.T) {
	t.Parallel()
	cfg := validCfg(t)
	cfg.ActorID = uuid.Nil
	if _, err := usecase.NewIssueCourtesyGrantService(newFakeCourtesyRepo(), cfg); err == nil {
		t.Fatal("want error for uuid.Nil actor, got nil")
	}
}

// ---------------------------------------------------------------------------
// Issue — happy path, idempotency, disabled flag, validation
// ---------------------------------------------------------------------------

func TestIssue_HappyPath(t *testing.T) {
	t.Parallel()
	repo := newFakeCourtesyRepo()
	svc, err := usecase.NewIssueCourtesyGrantService(repo, validCfg(t))
	if err != nil {
		t.Fatalf("NewIssueCourtesyGrantService: %v", err)
	}
	tid := uuid.New()
	got, err := svc.Issue(context.Background(), tid)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !got.Granted {
		t.Errorf("first call: Granted=false, want true")
	}
	if got.WalletID == uuid.Nil || got.GrantID == uuid.Nil {
		t.Errorf("first call: missing IDs: %+v", got)
	}
}

func TestIssue_NoOpOnSecondCall(t *testing.T) {
	t.Parallel()
	repo := newFakeCourtesyRepo()
	svc, err := usecase.NewIssueCourtesyGrantService(repo, validCfg(t))
	if err != nil {
		t.Fatalf("NewIssueCourtesyGrantService: %v", err)
	}
	tid := uuid.New()
	first, err := svc.Issue(context.Background(), tid)
	if err != nil {
		t.Fatalf("Issue#1: %v", err)
	}
	second, err := svc.Issue(context.Background(), tid)
	if err != nil {
		t.Fatalf("Issue#2: %v", err)
	}
	if second.Granted {
		t.Error("second call: Granted=true, want false (idempotent no-op)")
	}
	if second.WalletID != first.WalletID || second.GrantID != first.GrantID {
		t.Errorf("second call IDs drifted: first=%+v second=%+v", first, second)
	}
}

func TestIssue_DisabledReturnsSentinel(t *testing.T) {
	t.Parallel()
	repo := newFakeCourtesyRepo()
	cfg := validCfg(t)
	cfg.Disabled = true
	svc, err := usecase.NewIssueCourtesyGrantService(repo, cfg)
	if err != nil {
		t.Fatalf("NewIssueCourtesyGrantService: %v", err)
	}
	_, err = svc.Issue(context.Background(), uuid.New())
	if !errors.Is(err, wallet.ErrCourtesyGrantDisabled) {
		t.Fatalf("Issue disabled: got %v, want ErrCourtesyGrantDisabled", err)
	}
	if repo.calls.Load() != 0 {
		t.Errorf("disabled flag should short-circuit before repo call; got %d calls", repo.calls.Load())
	}
}

func TestIssue_RejectsZeroTenant(t *testing.T) {
	t.Parallel()
	svc, err := usecase.NewIssueCourtesyGrantService(newFakeCourtesyRepo(), validCfg(t))
	if err != nil {
		t.Fatalf("NewIssueCourtesyGrantService: %v", err)
	}
	_, err = svc.Issue(context.Background(), uuid.Nil)
	if !errors.Is(err, wallet.ErrZeroTenant) {
		t.Fatalf("Issue(uuid.Nil): got %v, want ErrZeroTenant", err)
	}
}

func TestIssue_PropagatesRepoError(t *testing.T) {
	t.Parallel()
	repo := newFakeCourtesyRepo()
	sentinel := errors.New("boom")
	repo.failNext = sentinel
	svc, err := usecase.NewIssueCourtesyGrantService(repo, validCfg(t))
	if err != nil {
		t.Fatalf("NewIssueCourtesyGrantService: %v", err)
	}
	_, err = svc.Issue(context.Background(), uuid.New())
	if !errors.Is(err, sentinel) {
		t.Fatalf("Issue: got %v, want sentinel", err)
	}
}

func TestIssue_RespectsContextCancel(t *testing.T) {
	t.Parallel()
	repo := newFakeCourtesyRepo()
	svc, err := usecase.NewIssueCourtesyGrantService(repo, validCfg(t))
	if err != nil {
		t.Fatalf("NewIssueCourtesyGrantService: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = svc.Issue(ctx, uuid.New())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Issue with cancelled ctx: got %v, want context.Canceled", err)
	}
}

func TestAmount_ReportsConfigured(t *testing.T) {
	t.Parallel()
	cfg := validCfg(t)
	cfg.Amount = 42_000
	svc, err := usecase.NewIssueCourtesyGrantService(newFakeCourtesyRepo(), cfg)
	if err != nil {
		t.Fatalf("NewIssueCourtesyGrantService: %v", err)
	}
	if got := svc.Amount(); got != 42_000 {
		t.Errorf("Amount() = %d, want 42000", got)
	}
}

// ---------------------------------------------------------------------------
// Concurrency — 50 callers race on the same tenant; exactly one wins,
// 49 see Granted=false. The fake serialises through a sync.Mutex so
// this is the use-case-level check; the adapter integration test
// covers the real Postgres race.
// ---------------------------------------------------------------------------

func TestIssue_FiftyConcurrentSameTenant(t *testing.T) {
	t.Parallel()
	repo := newFakeCourtesyRepo()
	svc, err := usecase.NewIssueCourtesyGrantService(repo, validCfg(t))
	if err != nil {
		t.Fatalf("NewIssueCourtesyGrantService: %v", err)
	}
	tid := uuid.New()
	const N = 50

	var (
		wins   atomic.Int64
		noOps  atomic.Int64
		errs   atomic.Int64
		wg     sync.WaitGroup
		start  = make(chan struct{})
		seenID atomic.Pointer[uuid.UUID]
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			out, err := svc.Issue(context.Background(), tid)
			if err != nil {
				errs.Add(1)
				return
			}
			if out.Granted {
				wins.Add(1)
			} else {
				noOps.Add(1)
			}
			// Every caller must observe the same wallet id, regardless
			// of who won the race.
			if prev := seenID.Load(); prev == nil {
				wid := out.WalletID
				seenID.CompareAndSwap(nil, &wid)
			} else if *prev != out.WalletID {
				t.Errorf("wallet id mismatch across callers: prev=%s now=%s", *prev, out.WalletID)
			}
		}()
	}
	close(start)
	wg.Wait()

	if errs.Load() != 0 {
		t.Fatalf("unexpected errors from concurrent Issue: %d", errs.Load())
	}
	if wins.Load() != 1 {
		t.Errorf("wins = %d, want exactly 1", wins.Load())
	}
	if noOps.Load() != N-1 {
		t.Errorf("noOps = %d, want %d", noOps.Load(), N-1)
	}
}
