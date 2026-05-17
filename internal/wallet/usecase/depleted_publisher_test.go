package usecase_test

// Service-level coverage for the SIN-62934 publisher hook: a successful
// Commit that brings the wallet balance to zero MUST emit one
// BalanceDepletedEvent with the right shape; non-depleting commits MUST
// stay silent; publisher errors MUST be swallowed (best-effort
// contract — the transaction has already committed).
//
// The tests use the same fakeRepo as the rest of the package so the
// fake matches the postgres adapter's invariants (SELECT…FOR UPDATE +
// version check + UNIQUE idempotency_key).

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/wallet/usecase"
)

// recordingPublisher captures every PublishBalanceDepleted call so a
// test can assert on the event payload + invocation count.
type recordingPublisher struct {
	mu     sync.Mutex
	events []wallet.BalanceDepletedEvent
	err    error
}

func (p *recordingPublisher) PublishBalanceDepleted(_ context.Context, evt wallet.BalanceDepletedEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, evt)
	return p.err
}

func (p *recordingPublisher) snapshot() []wallet.BalanceDepletedEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]wallet.BalanceDepletedEvent, len(p.events))
	copy(out, p.events)
	return out
}

func newSvcWithPublisher(t *testing.T, repo wallet.Repository, pub wallet.BalanceDepletedPublisher, opts ...usecase.Option) *usecase.Service {
	t.Helper()
	all := append([]usecase.Option{
		usecase.WithBalanceDepletedPublisher(pub),
		usecase.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}, opts...)
	svc, err := usecase.NewService(repo, func() time.Time { return fixedTime }, all...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestCommit_DepletesBalance_PublishesEvent(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 50, fixedTime)
	pub := &recordingPublisher{}
	svc := newSvcWithPublisher(t, repo, pub)

	res, err := svc.Reserve(context.Background(), tid, 50, "rsv-zero")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := svc.Commit(context.Background(), res, 50, "cmt-zero"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got := pub.snapshot()
	if len(got) != 1 {
		t.Fatalf("publish count = %d, want 1", len(got))
	}
	evt := got[0]
	if evt.TenantID != tid {
		t.Errorf("event TenantID = %s, want %s", evt.TenantID, tid)
	}
	if evt.PolicyScope != usecase.DefaultBalanceDepletedPolicyScope {
		t.Errorf("event PolicyScope = %q, want %q", evt.PolicyScope, usecase.DefaultBalanceDepletedPolicyScope)
	}
	if evt.LastChargeTokens != 50 {
		t.Errorf("event LastChargeTokens = %d, want 50", evt.LastChargeTokens)
	}
	if !evt.OccurredAt.Equal(fixedTime) {
		t.Errorf("event OccurredAt = %s, want %s", evt.OccurredAt, fixedTime)
	}
	if evt.OccurredAt.Location() != time.UTC {
		t.Errorf("event OccurredAt loc = %s, want UTC (publisher must force UTC for stable wire format)", evt.OccurredAt.Location())
	}
}

func TestCommit_NonDepletingCommit_DoesNotPublish(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 100, fixedTime)
	pub := &recordingPublisher{}
	svc := newSvcWithPublisher(t, repo, pub)

	res, _ := svc.Reserve(context.Background(), tid, 50, "rsv")
	if err := svc.Commit(context.Background(), res, 50, "cmt"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := len(pub.snapshot()); got != 0 {
		t.Errorf("publish count = %d, want 0 (balance still 50, not depleted)", got)
	}
}

func TestCommit_PartialCommitToZero_PublishesEvent(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 30, fixedTime)
	pub := &recordingPublisher{}
	svc := newSvcWithPublisher(t, repo, pub)

	res, _ := svc.Reserve(context.Background(), tid, 30, "rsv-partial")
	// Commit the full reserved amount — the balance hits exactly zero.
	if err := svc.Commit(context.Background(), res, 30, "cmt-partial"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got := pub.snapshot()
	if len(got) != 1 {
		t.Fatalf("publish count = %d, want 1", len(got))
	}
	if got[0].LastChargeTokens != 30 {
		t.Errorf("LastChargeTokens = %d, want 30 (actualAmount, not reservation amount)", got[0].LastChargeTokens)
	}
}

func TestCommit_PublisherError_DoesNotFailCommit(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 50, fixedTime)
	pub := &recordingPublisher{err: errors.New("nats: connection refused")}
	svc := newSvcWithPublisher(t, repo, pub)

	res, _ := svc.Reserve(context.Background(), tid, 50, "rsv")
	if err := svc.Commit(context.Background(), res, 50, "cmt"); err != nil {
		t.Fatalf("Commit failed because publisher errored: %v (best-effort contract violated)", err)
	}

	// The transaction must have landed even though the broker rejected.
	bal, rsv, _ := repo.snapshotBalance(res.WalletID)
	if bal != 0 || rsv != 0 {
		t.Errorf("post-Commit state with broker error: bal=%d rsv=%d, want 0/0", bal, rsv)
	}
	if got := len(pub.snapshot()); got != 1 {
		t.Errorf("publish attempt count = %d, want 1 (best-effort still attempts once)", got)
	}
}

func TestCommit_RetriedCommit_DoesNotRePublish(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 50, fixedTime)
	pub := &recordingPublisher{}
	svc := newSvcWithPublisher(t, repo, pub)

	res, _ := svc.Reserve(context.Background(), tid, 50, "rsv")
	if err := svc.Commit(context.Background(), res, 50, "cmt"); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	if err := svc.Commit(context.Background(), res, 50, "cmt"); err != nil {
		t.Fatalf("retry Commit: %v", err)
	}
	// The second call short-circuits via the idempotency lookup BEFORE
	// reaching ApplyWithLock; therefore it MUST NOT publish a duplicate
	// event. JetStream dedup at the adapter is a defense-in-depth
	// layer, but the use-case itself owes us at-most-once on retry.
	if got := len(pub.snapshot()); got != 1 {
		t.Errorf("publish count after idempotent retry = %d, want 1", got)
	}
}

func TestCommit_DefaultNoOpPublisher_DoesNotPanic(t *testing.T) {
	t.Parallel()
	// NewService without WithBalanceDepletedPublisher uses
	// NoOpBalanceDepletedPublisher; the depletion path must still
	// fire without crashing.
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 50, fixedTime)
	svc := newSvc(t, repo)

	res, _ := svc.Reserve(context.Background(), tid, 50, "rsv")
	if err := svc.Commit(context.Background(), res, 50, "cmt"); err != nil {
		t.Fatalf("Commit with default publisher: %v", err)
	}
}

func TestWithBalanceDepletedPolicyScope_OverridesDefault(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 50, fixedTime)
	pub := &recordingPublisher{}
	svc := newSvcWithPublisher(t, repo, pub, usecase.WithBalanceDepletedPolicyScope("policy:premium"))

	res, _ := svc.Reserve(context.Background(), tid, 50, "rsv")
	if err := svc.Commit(context.Background(), res, 50, "cmt"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got := pub.snapshot()
	if len(got) != 1 {
		t.Fatalf("publish count = %d, want 1", len(got))
	}
	if got[0].PolicyScope != "policy:premium" {
		t.Errorf("PolicyScope = %q, want %q (option override ignored)", got[0].PolicyScope, "policy:premium")
	}
}

func TestWithBalanceDepletedPolicyScope_EmptyKeepsDefault(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 50, fixedTime)
	pub := &recordingPublisher{}
	svc := newSvcWithPublisher(t, repo, pub, usecase.WithBalanceDepletedPolicyScope(""))

	res, _ := svc.Reserve(context.Background(), tid, 50, "rsv")
	if err := svc.Commit(context.Background(), res, 50, "cmt"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := pub.snapshot()[0].PolicyScope; got != usecase.DefaultBalanceDepletedPolicyScope {
		t.Errorf("PolicyScope = %q, want default %q (empty override should be ignored)", got, usecase.DefaultBalanceDepletedPolicyScope)
	}
}

func TestWithBalanceDepletedPublisher_NilKeepsNoOp(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 50, fixedTime)
	svc, err := usecase.NewService(repo, func() time.Time { return fixedTime },
		usecase.WithBalanceDepletedPublisher(nil),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// A nil publisher option must not clobber the no-op default; the
	// Commit path on a depleting wallet still completes cleanly.
	res, err := svc.Reserve(context.Background(), tid, 50, "rsv")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := svc.Commit(context.Background(), res, 50, "cmt"); err != nil {
		t.Fatalf("Commit with no-op publisher: %v", err)
	}
}

func TestWithLogger_NilKeepsDefault(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tid := uuid.New()
	repo.seed(tid, 50, fixedTime)
	// nil logger option must be tolerated — slog.Default is the fallback.
	svc, err := usecase.NewService(repo, func() time.Time { return fixedTime },
		usecase.WithLogger(nil),
		usecase.WithBalanceDepletedPublisher(&recordingPublisher{err: errors.New("boom")}),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	res, _ := svc.Reserve(context.Background(), tid, 50, "rsv")
	if err := svc.Commit(context.Background(), res, 50, "cmt"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// TestCommit_NoPanicOnNilOption guards the variadic NewService signature
// against a caller passing literal nil entries (e.g. ...Option from a
// slice that happens to be partially uninitialised).
func TestCommit_NoPanicOnNilOption(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	svc, err := usecase.NewService(repo, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewService(nil options): %v", err)
	}
	if svc == nil {
		t.Fatal("svc is nil")
	}
}
