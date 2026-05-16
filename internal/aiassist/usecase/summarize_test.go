package usecase_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aiassist"
	"github.com/pericles-luz/crm/internal/aiassist/usecase"
	"github.com/pericles-luz/crm/internal/wallet"
)

func newServiceForTest(
	t *testing.T,
	repo aiassist.SummaryRepository,
	w aiassist.WalletClient,
	llm aiassist.LLMClient,
	pol aiassist.PolicyResolver,
	clock *fixedClock,
) *usecase.Service {
	t.Helper()
	cfg := usecase.Config{
		Repo:   repo,
		Wallet: w,
		LLM:    llm,
		Policy: pol,
		Clock:  clock.Now,
	}
	s, err := usecase.NewService(cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

func defaultRequest() usecase.SummarizeRequest {
	return usecase.SummarizeRequest{
		TenantID:       uuid.New(),
		ConversationID: uuid.New(),
		RequestID:      "req-1",
		Prompt:         strings.Repeat("hello world ", 50),
		Scope:          aiassist.Scope{},
	}
}

func TestNewService_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	base := usecase.Config{
		Repo:   newFakeRepo(),
		Wallet: newFakeWallet(1_000_000),
		LLM:    &fakeLLM{},
		Policy: &fakePolicy{policy: defaultPolicy()},
	}

	cases := []struct {
		name   string
		mutate func(c *usecase.Config)
	}{
		{"nil repo", func(c *usecase.Config) { c.Repo = nil }},
		{"nil wallet", func(c *usecase.Config) { c.Wallet = nil }},
		{"nil llm", func(c *usecase.Config) { c.LLM = nil }},
		{"nil policy", func(c *usecase.Config) { c.Policy = nil }},
		{"limiter without bucket", func(c *usecase.Config) { c.RateLimiter = &fakeRateLimiter{allow: true} }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := base
			tc.mutate(&cfg)
			if _, err := usecase.NewService(cfg); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestNewService_DefaultClockAndTTL(t *testing.T) {
	t.Parallel()
	cfg := usecase.Config{
		Repo:   newFakeRepo(),
		Wallet: newFakeWallet(1),
		LLM:    &fakeLLM{},
		Policy: &fakePolicy{policy: defaultPolicy()},
	}
	if _, err := usecase.NewService(cfg); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
}

func TestSummarize_RejectsInvalidInput(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{}
	pol := &fakePolicy{policy: defaultPolicy()}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	cases := []struct {
		name string
		mut  func(*usecase.SummarizeRequest)
		want error
	}{
		{"zero tenant", func(r *usecase.SummarizeRequest) { r.TenantID = uuid.Nil }, aiassist.ErrZeroTenant},
		{"zero conversation", func(r *usecase.SummarizeRequest) { r.ConversationID = uuid.Nil }, aiassist.ErrZeroConversation},
		{"empty request id", func(r *usecase.SummarizeRequest) { r.RequestID = "" }, aiassist.ErrEmptyRequestID},
		{"empty prompt", func(r *usecase.SummarizeRequest) { r.Prompt = "" }, aiassist.ErrEmptyPrompt},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := defaultRequest()
			tc.mut(&req)
			_, err := svc.Summarize(context.Background(), req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if llm.callCount() != 0 {
		t.Fatalf("LLM must not be called on invalid input; got %d calls", llm.callCount())
	}
}

func TestSummarize_BlockedByPolicy(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{}

	cases := []struct {
		name   string
		policy aiassist.Policy
	}{
		{"ai disabled", aiassist.Policy{AIEnabled: false, OptIn: true, Model: "m"}},
		{"opt out", aiassist.Policy{AIEnabled: true, OptIn: false, Model: "m"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pol := &fakePolicy{policy: tc.policy}
			svc := newServiceForTest(t, repo, w, llm, pol, clock)
			_, err := svc.Summarize(context.Background(), defaultRequest())
			if !errors.Is(err, aiassist.ErrAIDisabled) {
				t.Fatalf("err = %v, want ErrAIDisabled", err)
			}
		})
	}
	if w.reserveCalls != 0 || llm.callCount() != 0 {
		t.Fatalf("policy block must short-circuit before reserve/llm; reserve=%d, llm=%d", w.reserveCalls, llm.callCount())
	}
}

func TestSummarize_PolicyResolveErrorPropagated(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	pol := &fakePolicy{policy: defaultPolicy(), err: errSentinel}
	svc := newServiceForTest(t, newFakeRepo(), newFakeWallet(1_000_000), &fakeLLM{}, pol, clock)
	_, err := svc.Summarize(context.Background(), defaultRequest())
	if err == nil || !errors.Is(err, errSentinel) {
		t.Fatalf("expected policy error to propagate, got %v", err)
	}
}

func TestSummarize_RateLimitDeny(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{}
	pol := &fakePolicy{policy: defaultPolicy()}
	limiter := &fakeRateLimiter{allow: false}
	cfg := usecase.Config{
		Repo: repo, Wallet: w, LLM: llm, Policy: pol,
		Clock:           clock.Now,
		RateLimiter:     limiter,
		RateLimitBucket: "aiassist:summarize",
	}
	svc, err := usecase.NewService(cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = svc.Summarize(context.Background(), defaultRequest())
	if !errors.Is(err, aiassist.ErrLLMUnavailable) {
		t.Fatalf("expected ErrLLMUnavailable on deny, got %v", err)
	}
	if w.reserveCalls != 0 || llm.callCount() != 0 {
		t.Fatalf("limiter deny must short-circuit; reserve=%d, llm=%d", w.reserveCalls, llm.callCount())
	}
}

func TestSummarize_RateLimitError(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	limiter := &fakeRateLimiter{allow: false, err: errSentinel}
	cfg := usecase.Config{
		Repo: newFakeRepo(), Wallet: newFakeWallet(1_000_000), LLM: &fakeLLM{},
		Policy:          &fakePolicy{policy: defaultPolicy()},
		Clock:           clock.Now,
		RateLimiter:     limiter,
		RateLimitBucket: "aiassist:summarize",
	}
	svc, err := usecase.NewService(cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = svc.Summarize(context.Background(), defaultRequest())
	if !errors.Is(err, aiassist.ErrLLMUnavailable) {
		t.Fatalf("expected ErrLLMUnavailable wrap, got %v", err)
	}
}

func TestSummarize_CacheHitSkipsLLM(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "fresh", TokensIn: 50, TokensOut: 25}}
	pol := &fakePolicy{policy: defaultPolicy()}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	req := defaultRequest()
	resp, err := svc.Summarize(context.Background(), req)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if resp.CacheHit {
		t.Fatalf("first call should be MISS")
	}
	if w.commitCalls != 1 {
		t.Fatalf("first call should commit; got commits=%d", w.commitCalls)
	}
	// retry; cache should hit
	clock.add(1 * time.Hour)
	req.RequestID = "req-2"
	resp2, err := svc.Summarize(context.Background(), req)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !resp2.CacheHit {
		t.Fatalf("second call should be HIT")
	}
	if llm.callCount() != 1 {
		t.Fatalf("cache hit should skip LLM; got %d calls", llm.callCount())
	}
	if w.commitCalls != 1 {
		t.Fatalf("cache hit should skip commit; got commits=%d", w.commitCalls)
	}
}

func TestSummarize_InsufficientBalance(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	repo := newFakeRepo()
	w := newFakeWallet(1) // 1 token total — far below estimate
	llm := &fakeLLM{}
	pol := &fakePolicy{policy: defaultPolicy()}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	_, err := svc.Summarize(context.Background(), defaultRequest())
	if !errors.Is(err, aiassist.ErrInsufficientBalance) {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
	if llm.callCount() != 0 {
		t.Fatalf("LLM must not be called on reserve failure")
	}
}

func TestSummarize_LLMErrorReleasesReservation(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{err: errSentinel}
	pol := &fakePolicy{policy: defaultPolicy()}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	_, err := svc.Summarize(context.Background(), defaultRequest())
	if !errors.Is(err, aiassist.ErrLLMUnavailable) {
		t.Fatalf("expected ErrLLMUnavailable wrap, got %v", err)
	}
	bal, reserved, commits, releases := w.snapshot()
	if bal != 1_000_000 {
		t.Fatalf("balance should be intact after release; got %d", bal)
	}
	if reserved != 0 {
		t.Fatalf("reservation must be released; got reserved=%d", reserved)
	}
	if commits != 0 || releases != 1 {
		t.Fatalf("commits=%d releases=%d, want 0/1", commits, releases)
	}
}

func TestSummarize_CommitsClampedToReservation(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	// LLM reports MORE tokens than the reservation budget. The use
	// case must clamp so wallet.Commit succeeds.
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "ok", TokensIn: 10_000, TokensOut: 10_000}}
	pol := &fakePolicy{policy: defaultPolicy()}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	resp, err := svc.Summarize(context.Background(), defaultRequest())
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if resp.CacheHit {
		t.Fatalf("first call should miss")
	}
	// reservation budget = EstimateReservation(prompt, model, 256)
	// prompt is 600 chars / 4 = 150 + 256 = 406. Actual reported = 20_000.
	bal, _, commits, _ := w.snapshot()
	if commits != 1 {
		t.Fatalf("expected 1 commit, got %d", commits)
	}
	deducted := int64(1_000_000) - bal
	if deducted > 600 {
		t.Fatalf("commit must clamp to reservation amount; deducted=%d", deducted)
	}
}

func TestSummarize_IdempotentReplaySharesReservation(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "summary", TokensIn: 50, TokensOut: 30}}
	pol := &fakePolicy{policy: defaultPolicy()}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	req := defaultRequest()
	if _, err := svc.Summarize(context.Background(), req); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Force an explicit invalidation so the second call cannot hit the
	// cache — that way we can prove the wallet's idempotency contract
	// is still in play when the same request_id is replayed.
	if err := svc.Invalidate(context.Background(), req.TenantID, req.ConversationID); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if _, err := svc.Summarize(context.Background(), req); err != nil {
		t.Fatalf("replay: %v", err)
	}
	// Reserve was called twice (one Reserve per Summarize), but the
	// second call short-circuited on the idempotency key.
	if w.reserveCalls != 2 {
		t.Fatalf("reserveCalls = %d, want 2", w.reserveCalls)
	}
	_, reserved, _, _ := w.snapshot()
	if reserved != 0 {
		t.Fatalf("reserved leak after replay; got reserved=%d", reserved)
	}
}

func TestSummarize_PassesMaxTokensAndModelToLLM(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "ok", TokensIn: 1, TokensOut: 1}}
	pol := &fakePolicy{policy: aiassist.Policy{
		AIEnabled: true, OptIn: true,
		Model: "anthropic/claude-haiku-4.5", MaxOutputTokens: 333,
	}}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	req := defaultRequest()
	if _, err := svc.Summarize(context.Background(), req); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if llm.lastReq.Model != "anthropic/claude-haiku-4.5" {
		t.Fatalf("LLM model = %q, want claude-haiku-4.5", llm.lastReq.Model)
	}
	if llm.lastReq.MaxTokens != 333 {
		t.Fatalf("LLM MaxTokens = %d, want 333", llm.lastReq.MaxTokens)
	}
	wantKey := req.TenantID.String() + ":" + req.ConversationID.String() + ":" + req.RequestID
	if llm.lastReq.IdempotencyKey != wantKey {
		t.Fatalf("IdempotencyKey = %q, want %q", llm.lastReq.IdempotencyKey, wantKey)
	}
}

func TestSummarize_RepoSaveErrorSurfacedAfterCommit(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	repo := newFakeRepo()
	repo.saveErr = errSentinel
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "ok", TokensIn: 1, TokensOut: 1}}
	pol := &fakePolicy{policy: defaultPolicy()}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	_, err := svc.Summarize(context.Background(), defaultRequest())
	if err == nil || !errors.Is(err, errSentinel) {
		t.Fatalf("expected Save error to surface, got %v", err)
	}
	_, _, commits, releases := w.snapshot()
	// commit ran before save; on save failure the wallet is already
	// debited and we do NOT compensate (audit trail / F37 picks it up)
	if commits != 1 || releases != 0 {
		t.Fatalf("commits=%d releases=%d, want 1/0", commits, releases)
	}
}

func TestSummarize_RepoGetErrorPropagated(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	repo := newFakeRepo()
	repo.getErr = errSentinel
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{}
	pol := &fakePolicy{policy: defaultPolicy()}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	_, err := svc.Summarize(context.Background(), defaultRequest())
	if err == nil || !errors.Is(err, errSentinel) {
		t.Fatalf("expected GetLatestValid error to surface, got %v", err)
	}
	if w.reserveCalls != 0 {
		t.Fatalf("reserve must not be called when cache lookup fails; got %d", w.reserveCalls)
	}
}

func TestSummarize_WalletReserveOtherError(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	w := newFakeWallet(1_000_000)
	w.reserveErr = errSentinel
	svc := newServiceForTest(t, newFakeRepo(), w, &fakeLLM{}, &fakePolicy{policy: defaultPolicy()}, clock)
	_, err := svc.Summarize(context.Background(), defaultRequest())
	if err == nil || errors.Is(err, aiassist.ErrInsufficientBalance) {
		t.Fatalf("non-ErrInsufficientFunds reserve error should propagate raw, got %v", err)
	}
}

func TestSummarize_WalletCommitErrorSurfaced(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	w := newFakeWallet(1_000_000)
	w.commitErr = errSentinel
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "ok", TokensIn: 1, TokensOut: 1}}
	svc := newServiceForTest(t, newFakeRepo(), w, llm, &fakePolicy{policy: defaultPolicy()}, clock)
	_, err := svc.Summarize(context.Background(), defaultRequest())
	if err == nil || !errors.Is(err, errSentinel) {
		t.Fatalf("expected commit error to surface, got %v", err)
	}
}

func TestSummarize_WalletReleaseErrorIncludesOriginal(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	w := newFakeWallet(1_000_000)
	w.releaseErr = errSentinel
	llm := &fakeLLM{err: errors.New("llm 500")}
	svc := newServiceForTest(t, newFakeRepo(), w, llm, &fakePolicy{policy: defaultPolicy()}, clock)
	_, err := svc.Summarize(context.Background(), defaultRequest())
	if err == nil {
		t.Fatalf("expected error when both LLM and release fail")
	}
	// Error must mention both
	if !strings.Contains(err.Error(), "release failed") {
		t.Fatalf("error should mention release failure: %v", err)
	}
}

func TestInvalidate_ValidationAndIdempotent(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	repo := newFakeRepo()
	svc := newServiceForTest(t, repo, newFakeWallet(1_000_000), &fakeLLM{}, &fakePolicy{policy: defaultPolicy()}, clock)
	ctx := context.Background()

	if err := svc.Invalidate(ctx, uuid.Nil, uuid.New()); !errors.Is(err, aiassist.ErrZeroTenant) {
		t.Fatalf("expected ErrZeroTenant, got %v", err)
	}
	if err := svc.Invalidate(ctx, uuid.New(), uuid.Nil); !errors.Is(err, aiassist.ErrZeroConversation) {
		t.Fatalf("expected ErrZeroConversation, got %v", err)
	}
	tenant := uuid.New()
	conv := uuid.New()
	if err := svc.Invalidate(ctx, tenant, conv); err != nil {
		t.Fatalf("idempotent invalidate on empty conv: %v", err)
	}
}

func TestInvalidate_PropagatesRepoError(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	repo := newFakeRepo()
	repo.invErr = errSentinel
	svc := newServiceForTest(t, repo, newFakeWallet(1_000_000), &fakeLLM{}, &fakePolicy{policy: defaultPolicy()}, clock)
	err := svc.Invalidate(context.Background(), uuid.New(), uuid.New())
	if err == nil || !errors.Is(err, errSentinel) {
		t.Fatalf("expected repo error, got %v", err)
	}
}

// TestSummarize_ConcurrentSpendDoesNotOverdraft mirrors AC #4 of
// SIN-62196 against the in-memory fake wallet so the unit test layer
// proves the use case never reserves more than the balance, even under
// many concurrent callers. The Postgres adapter test in
// internal/adapter/db/postgres/aiassist_adapter_test.go exercises the
// same shape against the real DB so the F30 SELECT … FOR UPDATE path
// participates too.
//
// We force pile-up by giving the LLM a short delay (~5ms), so 100
// goroutines accumulate outstanding reservations against a tiny
// 2_000-token wallet. With each reservation = 406 tokens, only 4
// concurrent reservations fit; the remaining 96 must observe
// ErrInsufficientBalance.
func TestSummarize_ConcurrentSpendDoesNotOverdraft(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	repo := newFakeRepo()
	w := newFakeWallet(2_000)
	llm := &fakeLLM{
		delay: 5 * time.Millisecond,
		customFn: func(req aiassist.LLMRequest) (aiassist.LLMResponse, error) {
			return aiassist.LLMResponse{Text: "summary", TokensIn: 200, TokensOut: 200}, nil
		},
	}
	pol := &fakePolicy{policy: defaultPolicy()}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	tenant := uuid.New()
	const N = 100
	results := make(chan error, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := usecase.SummarizeRequest{
				TenantID:       tenant,
				ConversationID: uuid.New(),
				RequestID:      uuid.NewString(),
				Prompt:         strings.Repeat("x", 600),
			}
			_, err := svc.Summarize(context.Background(), req)
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)

	var (
		ok           int
		insufficient int
	)
	for err := range results {
		if err == nil {
			ok++
			continue
		}
		if errors.Is(err, aiassist.ErrInsufficientBalance) {
			insufficient++
			continue
		}
		t.Fatalf("unexpected error: %v", err)
	}
	if ok+insufficient != N {
		t.Fatalf("ok+insufficient=%d, want %d", ok+insufficient, N)
	}
	bal, reserved, commits, _ := w.snapshot()
	if bal < 0 {
		t.Fatalf("balance went negative: %d", bal)
	}
	if reserved != 0 {
		t.Fatalf("reservations leaked: reserved=%d", reserved)
	}
	if int64(commits) != int64(ok) {
		t.Fatalf("commit count %d does not match successes %d", commits, ok)
	}
	if insufficient < 1 {
		t.Fatalf("expected at least one ErrInsufficientBalance under N=%d on a 2_000-token wallet; got 0", N)
	}
}

// TestSummarize_PolicyBlockedReasonExposed verifies the wrapped error
// keeps errors.Is(err, ErrAIDisabled) true and surfaces a human-
// readable reason for log triage.
func TestSummarize_PolicyBlockedReasonExposed(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	pol := &fakePolicy{policy: aiassist.Policy{AIEnabled: true, OptIn: false, Model: "m"}}
	svc := newServiceForTest(t, newFakeRepo(), newFakeWallet(1_000_000), &fakeLLM{}, pol, clock)
	_, err := svc.Summarize(context.Background(), defaultRequest())
	if !errors.Is(err, aiassist.ErrAIDisabled) {
		t.Fatalf("expected ErrAIDisabled wrap, got %v", err)
	}
	if !strings.Contains(err.Error(), "opt_in=false") {
		t.Fatalf("expected opt_in=false reason, got %q", err.Error())
	}
}

// TestSummarize_InvalidationRegeneratesCache covers AC #6: an
// invalidated cache row forces a fresh LLM call and a new committed
// summary. This is the unit-test mirror of the testpg version.
func TestSummarize_InvalidationRegeneratesCache(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "v1", TokensIn: 5, TokensOut: 5}}
	pol := &fakePolicy{policy: defaultPolicy()}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	req := defaultRequest()
	first, err := svc.Summarize(context.Background(), req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.CacheHit {
		t.Fatalf("first call should MISS")
	}

	// Cache hit confirmation
	req2 := req
	req2.RequestID = "req-2"
	second, err := svc.Summarize(context.Background(), req2)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.CacheHit {
		t.Fatalf("second call should HIT")
	}

	// Invalidate and regenerate
	if err := svc.Invalidate(context.Background(), req.TenantID, req.ConversationID); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	llm.resp = aiassist.LLMResponse{Text: "v2", TokensIn: 5, TokensOut: 5}
	req3 := req
	req3.RequestID = "req-3"
	third, err := svc.Summarize(context.Background(), req3)
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	if third.CacheHit {
		t.Fatalf("third call should MISS (post-invalidation)")
	}
	if third.Summary.Text != "v2" {
		t.Fatalf("regenerated summary text = %q, want v2", third.Summary.Text)
	}
	if llm.callCount() != 2 {
		t.Fatalf("LLM should be called twice (initial + post-invalidation); got %d", llm.callCount())
	}
}

// ensure the fakeWallet does not accidentally satisfy a phantom
// interface; the compile-time check is enough.
var _ aiassist.WalletClient = (*fakeWallet)(nil)
var _ aiassist.LLMClient = (*fakeLLM)(nil)
var _ aiassist.PolicyResolver = (*fakePolicy)(nil)
var _ aiassist.SummaryRepository = (*fakeRepo)(nil)
var _ aiassist.RateLimiter = (*fakeRateLimiter)(nil)

// Ensure the real wallet.Service satisfies the WalletClient port so
// cmd/server wiring compiles. We construct it elsewhere; here we just
// pin the type assertion.
var _ aiassist.WalletClient = (*walletStub)(nil)

type walletStub struct{}

func (walletStub) Reserve(_ context.Context, _ uuid.UUID, _ int64, _ string) (*wallet.Reservation, error) {
	return nil, nil
}
func (walletStub) Commit(_ context.Context, _ *wallet.Reservation, _ int64, _ string) error {
	return nil
}
func (walletStub) Release(_ context.Context, _ *wallet.Reservation, _ string) error { return nil }
