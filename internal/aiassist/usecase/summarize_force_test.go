package usecase_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aiassist"
)

// TestSummarize_ForceRegeneratesIgnoringCache covers SIN-65474 AC #1: a
// forced refresh must regenerate (call the LLM + commit the wallet)
// even when a fresh, valid cache row already exists. The non-forced
// retry of the same request still hits the cache, proving Force is the
// only thing that bypasses it.
func TestSummarize_ForceRegeneratesIgnoringCache(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC))
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "fresh", TokensIn: 50, TokensOut: 25}}
	pol := &fakePolicy{policy: defaultPolicy()}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	req := defaultRequest()
	if _, err := svc.Summarize(context.Background(), req); err != nil {
		t.Fatalf("seed call: %v", err)
	}
	if llm.callCount() != 1 || w.commitCalls != 1 {
		t.Fatalf("seed should generate once; llm=%d commits=%d", llm.callCount(), w.commitCalls)
	}

	// A plain retry hits the cache (no LLM, no commit).
	clock.add(1 * time.Minute)
	req.RequestID = "req-cache"
	resp, err := svc.Summarize(context.Background(), req)
	if err != nil {
		t.Fatalf("cache retry: %v", err)
	}
	if !resp.CacheHit {
		t.Fatalf("plain retry should be a cache HIT")
	}
	if llm.callCount() != 1 || w.commitCalls != 1 {
		t.Fatalf("cache hit must skip LLM+commit; llm=%d commits=%d", llm.callCount(), w.commitCalls)
	}

	// The forced refresh regenerates: LLM called again, wallet committed
	// again, CacheHit false.
	clock.add(1 * time.Minute)
	req.RequestID = "req-force"
	req.Force = true
	forced, err := svc.Summarize(context.Background(), req)
	if err != nil {
		t.Fatalf("forced refresh: %v", err)
	}
	if forced.CacheHit {
		t.Fatalf("forced refresh must NOT be a cache hit")
	}
	if llm.callCount() != 2 {
		t.Fatalf("forced refresh should call the LLM again; got %d", llm.callCount())
	}
	if w.commitCalls != 2 {
		t.Fatalf("forced refresh should commit the wallet again; got %d", w.commitCalls)
	}
}

// TestSummarize_ForceStillHonoursPolicyGate covers SIN-65474 AC #1's
// "must respect the existing AI policy gate" clause: Force bypasses the
// cache, never the deny-by-default policy. A disabled policy refuses the
// forced call without touching the LLM.
func TestSummarize_ForceStillHonoursPolicyGate(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{}
	disabled := defaultPolicy()
	disabled.AIEnabled = false
	pol := &fakePolicy{policy: disabled}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	req := defaultRequest()
	req.Force = true
	_, err := svc.Summarize(context.Background(), req)
	if err == nil {
		t.Fatalf("forced call against a disabled policy must error")
	}
	if llm.callCount() != 0 {
		t.Fatalf("policy gate must short-circuit before the LLM; got %d calls", llm.callCount())
	}
}

// TestLatestSummaryGeneratedAt covers the SIN-65474 staleness read side:
// a miss reports (zero, false); after a summary is committed the method
// returns its generated_at with exists == true.
func TestLatestSummaryGeneratedAt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	clock := newFixedClock(now)
	repo := newFakeRepo()
	w := newFakeWallet(1_000_000)
	llm := &fakeLLM{resp: aiassist.LLMResponse{Text: "fresh", TokensIn: 10, TokensOut: 5}}
	pol := &fakePolicy{policy: defaultPolicy()}
	svc := newServiceForTest(t, repo, w, llm, pol, clock)

	tenant := uuid.New()
	conv := uuid.New()

	// No summary yet → not stale-able.
	at, exists, err := svc.LatestSummaryGeneratedAt(context.Background(), tenant, conv)
	if err != nil {
		t.Fatalf("miss lookup: %v", err)
	}
	if exists || !at.IsZero() {
		t.Fatalf("expected (zero,false) on miss; got (%v,%v)", at, exists)
	}

	// Generate one.
	req := defaultRequest()
	req.TenantID = tenant
	req.ConversationID = conv
	if _, err := svc.Summarize(context.Background(), req); err != nil {
		t.Fatalf("seed summary: %v", err)
	}

	at, exists, err = svc.LatestSummaryGeneratedAt(context.Background(), tenant, conv)
	if err != nil {
		t.Fatalf("hit lookup: %v", err)
	}
	if !exists {
		t.Fatalf("expected exists == true after a summary is committed")
	}
	if !at.Equal(now) {
		t.Fatalf("generated_at = %v, want %v", at, now)
	}
}
