package iam

// SIN-62344 Login + Lockout integration unit tests. These cover the
// branches of the SIN-62341 acceptance criteria that do NOT need a
// real Postgres or Redis: the master alert path (AC #3) and the
// timing-equivalence regression (AC #4 — 50 samples, < 20% divergence).
//
// Postgres-backed integration tests for ACs #1, #2, #5 (11th attempt
// trips lockout, lockout survives Redis flush, lockout survives a
// simulated Redis Stop/Start) live alongside the postgres adapter in
// internal/adapter/db/postgres/login_lockout_integration_test.go so
// they can share the testpg harness.

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/ratelimit"
)

// inMemoryLimiter is the iam-test-only Allow implementation: a per-key
// sliding-window counter that approximates the Redis adapter's
// semantics without requiring a container. Each Allow call records a
// hit and reports whether the post-record count is within max. The
// Flush helper drops every counter — this is how AC #2 / AC #5
// simulate a Redis FLUSHALL.
type inMemoryLimiter struct {
	mu     sync.Mutex
	counts map[string]int
	calls  atomic.Int32
}

func newInMemoryLimiter() *inMemoryLimiter {
	return &inMemoryLimiter{counts: map[string]int{}}
}

func (l *inMemoryLimiter) Allow(_ context.Context, key string, _ time.Duration, max int) (bool, time.Duration, error) {
	l.calls.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts[key]++
	if l.counts[key] > max {
		return false, time.Second, nil
	}
	return true, 0, nil
}

// Flush drops every counter — equivalent to FLUSHALL on Redis.
func (l *inMemoryLimiter) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts = map[string]int{}
}

// inMemoryLockouts is a fake Lockouts that records Lock writes in a
// map. The IsLocked check is real-time aware so tests can assert
// expiry behaviour. Used for the Login-level master alert test
// (AC #3) — the postgres-backed version covers RLS + the audit row.
type inMemoryLockouts struct {
	mu   sync.Mutex
	rows map[uuid.UUID]time.Time
	now  func() time.Time
}

func newInMemoryLockouts() *inMemoryLockouts {
	return &inMemoryLockouts{rows: map[uuid.UUID]time.Time{}, now: func() time.Time { return time.Now().UTC() }}
}

func (l *inMemoryLockouts) Lock(_ context.Context, userID uuid.UUID, until time.Time, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows[userID] = until
	return nil
}
func (l *inMemoryLockouts) IsLocked(_ context.Context, userID uuid.UUID) (bool, time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	until, ok := l.rows[userID]
	if !ok || !until.After(l.now()) {
		return false, time.Time{}, nil
	}
	return true, until, nil
}
func (l *inMemoryLockouts) Clear(_ context.Context, userID uuid.UUID) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.rows, userID)
	return nil
}

// recordingAlerter is the test double for ratelimit.Alerter — records
// each Notify call so AC #3 can assert the master lockout fired one
// (and only one) alert.
type recordingAlerter struct {
	mu       sync.Mutex
	messages []string
	err      error
}

func (a *recordingAlerter) Notify(_ context.Context, msg string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = append(a.messages, msg)
	return a.err
}

func (a *recordingAlerter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.messages)
}

// masterPolicy returns the SIN-62341 m_login policy in a form the
// tests can inspect / tweak. Threshold=5 + AlertOnLock=true.
func masterPolicy(t *testing.T) ratelimit.Policy {
	t.Helper()
	policies, err := ratelimit.DefaultPolicies()
	if err != nil {
		t.Fatalf("DefaultPolicies: %v", err)
	}
	p, ok := policies["m_login"]
	if !ok {
		t.Fatal("m_login policy missing from DefaultPolicies")
	}
	return p
}

// loginPolicy returns the SIN-62341 login policy (Threshold=10).
func loginPolicy(t *testing.T) ratelimit.Policy {
	t.Helper()
	policies, err := ratelimit.DefaultPolicies()
	if err != nil {
		t.Fatalf("DefaultPolicies: %v", err)
	}
	p, ok := policies["login"]
	if !ok {
		t.Fatal("login policy missing from DefaultPolicies")
	}
	return p
}

// ---------------------------------------------------------------------------
// AC #3: master lockout fires the synchronous Slack alert.
// ---------------------------------------------------------------------------

func TestLogin_MasterLockout_FiresAlerterOnce(t *testing.T) {
	t.Parallel()
	svc, _, _ := newServiceForTest(t)
	svc.Lockouts = newInMemoryLockouts()
	svc.Limiter = newInMemoryLimiter()
	svc.LoginPolicy = masterPolicy(t)
	alerter := &recordingAlerter{}
	svc.Alerter = alerter

	ctx := context.Background()

	// m_login Threshold=5 → six wrong-password attempts; the 6th trips
	// the lockout. The first five must NOT fire the alerter.
	for i := 0; i < 5; i++ {
		_, err := svc.Login(ctx, "acme.crm.local", "alice@acme.test", "WRONG", nil, "", "")
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: err=%v want ErrInvalidCredentials", i+1, err)
		}
	}
	if alerter.count() != 0 {
		t.Fatalf("alerter fired %d times before threshold", alerter.count())
	}

	// 6th attempt: lockout writes + alert fires.
	_, err := svc.Login(ctx, "acme.crm.local", "alice@acme.test", "WRONG", nil, "", "")
	if !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("trip attempt: err=%v want ErrAccountLocked", err)
	}
	if got := alerter.count(); got != 1 {
		t.Fatalf("alerter.count() = %d, want 1", got)
	}

	// Subsequent attempts are pre-locked — they MUST NOT fire another
	// alert (the IsLocked branch short-circuits before
	// recordLoginFailure is reached).
	for i := 0; i < 3; i++ {
		_, err := svc.Login(ctx, "acme.crm.local", "alice@acme.test", "WRONG", nil, "", "")
		if !errors.Is(err, ErrAccountLocked) {
			t.Fatalf("post-lock attempt: err=%v want ErrAccountLocked", err)
		}
	}
	if got := alerter.count(); got != 1 {
		t.Fatalf("alerter fired %d times total, want 1 (pre-locked attempts must not re-alert)", got)
	}
}

// TestLogin_TenantLockout_DoesNotAlert covers the inverse of AC #3 —
// the tenant policy has AlertOnLock=false, so a tripped lockout
// MUST NOT call the wired Alerter.
func TestLogin_TenantLockout_DoesNotAlert(t *testing.T) {
	t.Parallel()
	svc, _, _ := newServiceForTest(t)
	svc.Lockouts = newInMemoryLockouts()
	svc.Limiter = newInMemoryLimiter()
	svc.LoginPolicy = loginPolicy(t)
	alerter := &recordingAlerter{}
	svc.Alerter = alerter

	ctx := context.Background()
	for i := 0; i < int(svc.LoginPolicy.Lockout.Threshold)+1; i++ {
		_, _ = svc.Login(ctx, "acme.crm.local", "alice@acme.test", "WRONG", nil, "", "")
	}
	if alerter.count() != 0 {
		t.Fatalf("tenant lockout fired alerter %d times — AlertOnLock=false", alerter.count())
	}
}

// TestLogin_AlerterError_DoesNotAbortLockout asserts that a Notify
// failure is logged but does not unwind the persisted Lock. The
// account_lockout row is the authoritative penalty; the alert is the
// notification side-effect.
func TestLogin_AlerterError_DoesNotAbortLockout(t *testing.T) {
	t.Parallel()
	svc, _, userID := newServiceForTest(t)
	lockouts := newInMemoryLockouts()
	svc.Lockouts = lockouts
	svc.Limiter = newInMemoryLimiter()
	svc.LoginPolicy = masterPolicy(t)
	svc.Alerter = &recordingAlerter{err: errors.New("boom: webhook 503")}

	ctx := context.Background()
	for i := 0; i < int(svc.LoginPolicy.Lockout.Threshold)+1; i++ {
		_, _ = svc.Login(ctx, "acme.crm.local", "alice@acme.test", "WRONG", nil, "", "")
	}
	locked, _, err := lockouts.IsLocked(ctx, userID)
	if err != nil {
		t.Fatalf("IsLocked: %v", err)
	}
	if !locked {
		t.Fatal("alerter error rolled back the lockout — must persist")
	}
}

// ---------------------------------------------------------------------------
// AC #4: login timing of unknown vs existing email diverges < 20%.
// 50 samples per branch.
// ---------------------------------------------------------------------------

func TestLogin_TimingEquivalence_UnknownVsExisting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing test in -short mode")
	}
	// No t.Parallel(): timing measurements MUST not be skewed by
	// concurrent argon2 derivations from sibling tests. The 50-sample
	// median is stable when this test owns the CPU for its duration.

	svc, _, _ := newServiceForTest(t)
	ctx := context.Background()

	const samples = 50

	// Interleave the two branches so any CPU-frequency / GC / OS
	// scheduling drift across the run distributes evenly over both
	// medians instead of biasing the second branch. Two warmup passes
	// (one per branch) prime caches and the argon2 memory allocator.
	for _, email := range []string{"alice@acme.test", "ghost@acme.test"} {
		_, _ = svc.Login(ctx, "acme.crm.local", email, "WRONG", nil, "", "")
	}

	wrongPwdSamples := make([]time.Duration, 0, samples)
	unknownSamples := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		start := time.Now()
		_, _ = svc.Login(ctx, "acme.crm.local", "alice@acme.test", "WRONG", nil, "", "")
		wrongPwdSamples = append(wrongPwdSamples, time.Since(start))

		start = time.Now()
		_, _ = svc.Login(ctx, "acme.crm.local", "ghost@acme.test", "WRONG", nil, "", "")
		unknownSamples = append(unknownSamples, time.Since(start))
	}
	wrongPwd := medianOf(wrongPwdSamples)
	unknown := medianOf(unknownSamples)

	// Two-sided ≤ 20% divergence per AC #4.
	deltaPct := percentDelta(wrongPwd, unknown)
	if deltaPct > 0.20 {
		t.Fatalf("AC #4: timing divergence = %.2f%% (wrongPwd=%v, unknown=%v) — must be < 20%%",
			deltaPct*100, wrongPwd, unknown)
	}
	t.Logf("AC #4: timing divergence = %.2f%% (wrongPwd=%v, unknown=%v) — within bound",
		deltaPct*100, wrongPwd, unknown)
}

// medianOf returns the median of samples. Uses sort + middle index;
// pure, no allocations beyond the sort copy.
func medianOf(samples []time.Duration) time.Duration {
	cp := append([]time.Duration(nil), samples...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	return cp[len(cp)/2]
}

// percentDelta returns the absolute fractional divergence of a vs b
// against max(a, b). Symmetric in a and b. Returns 0 if both are
// zero, 1 if exactly one is zero.
func percentDelta(a, b time.Duration) float64 {
	if a == 0 && b == 0 {
		return 0
	}
	hi, lo := a, b
	if lo > hi {
		hi, lo = lo, hi
	}
	if hi == 0 {
		return 1
	}
	return float64(hi-lo) / float64(hi)
}
