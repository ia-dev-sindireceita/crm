package hibp

import (
	"sync"
	"time"
)

// breakerState is the canonical 3-state circuit breaker (Hystrix-style).
//
//   - closed: requests flow to the upstream; consecutive failures count
//     toward the trip threshold.
//   - open: requests are short-circuited with ErrUnavailable so we do not
//     hammer a known-down upstream.
//   - half-open: after the cool-down window, a single probe is allowed
//     through. Success closes; failure re-opens.
type breakerState int

const (
	stateClosed breakerState = iota
	stateOpen
	stateHalfOpen
)

// breaker is a small, dependency-free circuit breaker. It is package
// internal so the public Client surface is the only thing tests touch —
// breaker behaviour is exercised through Client tests.
type breaker struct {
	mu        sync.Mutex
	state     breakerState
	failures  int
	openedAt  time.Time
	threshold int
	cooldown  time.Duration
	now       func() time.Time
}

func newBreaker(threshold int, cooldown time.Duration) *breaker {
	return &breaker{
		state:     stateClosed,
		threshold: threshold,
		cooldown:  cooldown,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// allow reports whether a request should be sent upstream. When the
// breaker is open and the cool-down has elapsed, the first caller is
// promoted to a half-open probe and gets allow=true.
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateClosed, stateHalfOpen:
		return true
	case stateOpen:
		if b.now().Sub(b.openedAt) >= b.cooldown {
			b.state = stateHalfOpen
			return true
		}
		return false
	}
	return false
}

// recordSuccess closes the breaker and resets the failure count.
func (b *breaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.state = stateClosed
}

// recordFailure increments the failure count; if it crosses the
// threshold (or we were half-open and just probed), the breaker trips.
func (b *breaker) recordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.state == stateHalfOpen || b.failures >= b.threshold {
		b.state = stateOpen
		b.openedAt = b.now()
	}
}
