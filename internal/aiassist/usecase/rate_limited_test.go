package usecase_test

// SIN-62908 — verifies that the multi-wrapped rate-limit deny exposes
// both aiassist.ErrLLMUnavailable (legacy callers) AND
// aiassist.ErrRateLimited (UI layer that needs to disambiguate from
// generic LLM unavailability). The existing TestSummarize_RateLimitDeny
// already covers the LLMUnavailable branch; this test adds the
// ErrRateLimited assertion without modifying the existing test.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/aiassist"
	"github.com/pericles-luz/crm/internal/aiassist/usecase"
)

func TestSummarize_RateLimitDeny_AlsoExposesErrRateLimited(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	cfg := usecase.Config{
		Repo:            newFakeRepo(),
		Wallet:          newFakeWallet(1_000_000),
		LLM:             &fakeLLM{},
		Policy:          &fakePolicy{policy: defaultPolicy()},
		Clock:           clock.Now,
		RateLimiter:     &fakeRateLimiter{allow: false},
		RateLimitBucket: "aiassist:summarize",
	}
	svc, err := usecase.NewService(cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = svc.Summarize(context.Background(), defaultRequest())
	if err == nil {
		t.Fatalf("expected error on rate-limit deny")
	}
	if !errors.Is(err, aiassist.ErrLLMUnavailable) {
		t.Errorf("errors.Is(err, ErrLLMUnavailable): false, want true")
	}
	if !errors.Is(err, aiassist.ErrRateLimited) {
		t.Errorf("errors.Is(err, ErrRateLimited): false, want true")
	}
}

// TestSummarize_RateLimitError_DoesNotMatchErrRateLimited proves the
// "limiter itself failed" branch keeps the legacy semantics: callers
// see ErrLLMUnavailable but NOT ErrRateLimited (which is reserved for
// the deny-by-policy branch).
func TestSummarize_RateLimitError_DoesNotMatchErrRateLimited(t *testing.T) {
	t.Parallel()
	clock := newFixedClock(time.Now())
	cfg := usecase.Config{
		Repo:            newFakeRepo(),
		Wallet:          newFakeWallet(1_000_000),
		LLM:             &fakeLLM{},
		Policy:          &fakePolicy{policy: defaultPolicy()},
		Clock:           clock.Now,
		RateLimiter:     &fakeRateLimiter{allow: false, err: errors.New("limiter offline")},
		RateLimitBucket: "aiassist:summarize",
	}
	svc, err := usecase.NewService(cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = svc.Summarize(context.Background(), defaultRequest())
	if err == nil {
		t.Fatalf("expected error on limiter failure")
	}
	if !errors.Is(err, aiassist.ErrLLMUnavailable) {
		t.Errorf("errors.Is(err, ErrLLMUnavailable): false, want true")
	}
	if errors.Is(err, aiassist.ErrRateLimited) {
		t.Errorf("errors.Is(err, ErrRateLimited): true, want false — limiter failure is not a rate-limit deny")
	}
}
