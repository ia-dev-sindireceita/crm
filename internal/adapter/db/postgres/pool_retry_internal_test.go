package postgres

import (
	"context"
	"errors"
	"testing"
	"time"
)

// errPingTest is the canned failure a fakePinger returns.
var errPingTest = errors.New("ping refused")

// fakePinger fails its first failFirst calls (with err, or errPingTest if
// err is nil) then succeeds on every call after that. It counts calls so
// tests can assert the attempt count.
type fakePinger struct {
	failFirst int
	err       error
	calls     int
}

func (f *fakePinger) Ping(context.Context) error {
	f.calls++
	if f.calls <= f.failFirst {
		if f.err != nil {
			return f.err
		}
		return errPingTest
	}
	return nil
}

// fastBackoff keeps the retry loop's sleeps negligible so success/exhaustion
// tests stay sub-millisecond; the budget alone bounds them.
const (
	fastInitial = 1 * time.Millisecond
	fastMax     = 1 * time.Millisecond
)

func TestPingWithRetry_SucceedsAfterFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		failFirst int
		wantCalls int
	}{
		{name: "succeeds first try", failFirst: 0, wantCalls: 1},
		{name: "one failure then success", failFirst: 1, wantCalls: 2},
		{name: "several failures then success", failFirst: 3, wantCalls: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fp := &fakePinger{failFirst: tt.failFirst}
			err := pingWithRetry(context.Background(), fp, defaultPingRetryBudget, fastInitial, fastMax)
			if err != nil {
				t.Fatalf("pingWithRetry: got %v, want nil", err)
			}
			if fp.calls != tt.wantCalls {
				t.Errorf("calls: got %d, want %d", fp.calls, tt.wantCalls)
			}
		})
	}
}

func TestPingWithRetry_BudgetExhausted(t *testing.T) {
	t.Parallel()
	// failFirst far exceeds anything the tiny budget can reach, so the loop
	// must give up and surface the last ping error.
	fp := &fakePinger{failFirst: 1 << 30}
	start := time.Now()
	err := pingWithRetry(context.Background(), fp, 10*time.Millisecond, fastInitial, fastMax)
	elapsed := time.Since(start)
	if !errors.Is(err, errPingTest) {
		t.Fatalf("err: got %v, want errPingTest", err)
	}
	if fp.calls < 1 {
		t.Errorf("calls: got %d, want at least 1", fp.calls)
	}
	if elapsed > time.Second {
		t.Errorf("elapsed %v exceeded a tiny budget by too much", elapsed)
	}
}

func TestPingWithRetry_ContextDeadlineDuringBackoff(t *testing.T) {
	t.Parallel()
	// ctx deadline is far shorter than the budget; a long initial backoff
	// guarantees the timer wait outlives the deadline so we exercise the
	// select on ctx.Done(). Must return promptly, not after the 10s budget.
	fp := &fakePinger{failFirst: 1 << 30}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := pingWithRetry(ctx, fp, 10*time.Second, 50*time.Millisecond, defaultPingMaxBackoff)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err: got %v, want context.DeadlineExceeded", err)
	}
	if elapsed > time.Second {
		t.Errorf("elapsed %v: did not return promptly on ctx deadline", elapsed)
	}
	if fp.calls < 1 {
		t.Errorf("calls: got %d, want at least one ping attempt", fp.calls)
	}
}

func TestPingWithRetry_ContextAlreadyCancelled(t *testing.T) {
	t.Parallel()
	// ctx cancelled before entry: return the ctx error without pinging.
	fp := &fakePinger{failFirst: 0}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := pingWithRetry(ctx, fp, defaultPingRetryBudget, fastInitial, fastMax)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err: got %v, want context.Canceled", err)
	}
	if fp.calls != 0 {
		t.Errorf("calls: got %d, want 0 (no ping after cancel)", fp.calls)
	}
}

// hangingPinger blocks until its (per-attempt) ctx is cancelled, then returns
// that ctx's error. It models a DB host that hangs at the TCP layer (slow
// DNS / no RST). Before the per-attempt-timeout fix, pingWithRetry passed the
// raw caller ctx straight to Ping, so with a deadline-less caller ctx this
// would block forever and the budget check was never reached.
type hangingPinger struct{ calls int }

func (h *hangingPinger) Ping(ctx context.Context) error {
	h.calls++
	<-ctx.Done()
	return ctx.Err()
}

func TestResolvePingRetryBudget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{name: "unset falls back to default", raw: "", want: defaultPingRetryBudget},
		{name: "valid duration is used", raw: "1ms", want: time.Millisecond},
		{name: "valid seconds is used", raw: "30s", want: 30 * time.Second},
		{name: "garbage falls back to default", raw: "not-a-duration", want: defaultPingRetryBudget},
		{name: "zero falls back to default", raw: "0s", want: defaultPingRetryBudget},
		{name: "negative falls back to default", raw: "-5s", want: defaultPingRetryBudget},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolvePingRetryBudget(tt.raw); got != tt.want {
				t.Errorf("resolvePingRetryBudget(%q): got %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestPingWithRetry_HangingPingBoundedByBudget(t *testing.T) {
	t.Parallel()
	// Caller ctx has NO deadline (mirrors production main / cmd/server
	// tests). Each Ping hangs; only the per-attempt timeout lets the loop
	// make progress and return within the budget instead of blocking
	// forever. A hanging host is retried like any other failure (literal
	// AC1) and bounded by the budget — without the per-attempt fix this test
	// would hang until the go-test timeout fired.
	hp := &hangingPinger{}
	start := time.Now()
	err := pingWithRetry(context.Background(), hp, 40*time.Millisecond, fastInitial, 5*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("err: got nil, want a non-nil timeout error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("elapsed %v: per-attempt timeout did not bound a hanging ping", elapsed)
	}
	if hp.calls < 1 {
		t.Errorf("calls: got %d, want at least one bounded ping attempt", hp.calls)
	}
}
