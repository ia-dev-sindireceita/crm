package webchat

import (
	"context"
	"strings"
	"sync"
	"time"
)

// WindowRateLimiter is a fixed-window, in-memory RateLimiter for the
// webchat public surface (ADR-0021 D5). It is the production limiter:
// unlike InMemoryRateLimiter (a permanent counter that never resets and
// is only fit for unit tests), each key gets a rolling window that
// refills once the window elapses.
//
// Limits are selected by the key prefix the handler builds:
//
//	wc.sess.<tenant>.<ip_hash>  → 10 sessions / minute  (D5 session create)
//	wc.msg.<session_id>         → 60 messages / minute  (D5 message)
//
// Fase 2 is single-instance, so an in-memory window is acceptable per
// ADR-0021 D5 ("Multi-instance sync via Redis fica para Fase 3"). The
// /24 anti-sybil bucket and the SSE connection caps in D5 are NOT yet
// enforced here — they are tracked as follow-up hardening for the
// SecurityEngineer review of the public surface.
type WindowRateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*windowBucket
	rules    []windowRule
	fallback windowRule
	now      func() time.Time
}

type windowRule struct {
	prefix string
	limit  int
	window time.Duration
}

type windowBucket struct {
	count   int
	resetAt time.Time
}

// maxWindowBuckets caps the live bucket map. Keys are per-session and
// per-ip_hash, so the map would otherwise grow unbounded over a long
// uptime; once the cap is crossed a new key triggers an opportunistic
// sweep of expired buckets (no background goroutine).
const maxWindowBuckets = 100_000

// NewWindowRateLimiter returns the production limiter wired with the
// ADR-0021 D5 defaults (10 session-creates/min, 60 messages/min, and a
// 60/min fallback for any other key).
func NewWindowRateLimiter() *WindowRateLimiter {
	return newWindowRateLimiterWithClock(time.Now)
}

// newWindowRateLimiterWithClock is the test seam; production passes
// time.Now.
func newWindowRateLimiterWithClock(now func() time.Time) *WindowRateLimiter {
	return &WindowRateLimiter{
		buckets: make(map[string]*windowBucket),
		rules: []windowRule{
			{prefix: "wc.sess.", limit: 10, window: time.Minute},
			{prefix: "wc.msg.", limit: 60, window: time.Minute},
		},
		fallback: windowRule{limit: 60, window: time.Minute},
		now:      now,
	}
}

func (l *WindowRateLimiter) ruleFor(key string) windowRule {
	for _, r := range l.rules {
		if strings.HasPrefix(key, r.prefix) {
			return r
		}
	}
	return l.fallback
}

// Allow implements RateLimiter. It increments the key's window counter
// and denies once the limit is exceeded; retryAfter reports the time
// until the current window resets.
func (l *WindowRateLimiter) Allow(_ context.Context, key string) (bool, time.Duration, error) {
	rule := l.ruleFor(key)
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxWindowBuckets {
			l.sweepExpiredLocked(now)
		}
		b = &windowBucket{}
		l.buckets[key] = b
	}
	if now.After(b.resetAt) || now.Equal(b.resetAt) {
		b.count = 0
		b.resetAt = now.Add(rule.window)
	}
	b.count++
	if b.count > rule.limit {
		return false, b.resetAt.Sub(now), nil
	}
	return true, 0, nil
}

// sweepExpiredLocked drops buckets whose window has elapsed. Caller
// holds l.mu.
func (l *WindowRateLimiter) sweepExpiredLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.After(b.resetAt) {
			delete(l.buckets, k)
		}
	}
}
