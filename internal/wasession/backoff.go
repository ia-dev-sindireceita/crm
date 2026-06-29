package wasession

import "time"

// Backoff is a capped exponential reconnect policy. attempt is 1-based: the
// first retry waits Base, the second 2*Base, then 4*Base, ... clamped to Max.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
}

// DefaultBackoff is the policy used when a Manager is built without an
// explicit one.
var DefaultBackoff = Backoff{Base: time.Second, Max: 2 * time.Minute}

// Delay returns the wait before the given 1-based retry attempt.
func (b Backoff) Delay(attempt int) time.Duration {
	base := b.Base
	if base <= 0 {
		base = DefaultBackoff.Base
	}
	max := b.Max
	if max <= 0 {
		max = DefaultBackoff.Max
	}
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}
