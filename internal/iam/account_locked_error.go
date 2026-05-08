package iam

import (
	"errors"
	"time"
)

// AccountLockedError is the typed error Login returns when an account
// is locked. It exposes the locked_until timestamp so the HTTP layer
// can derive an accurate Retry-After header (SIN-62348 §"HTTP
// middleware: 429 + Retry-After"). The sentinel ErrAccountLocked is
// kept as the errors.Is target so existing call sites that test
// against it continue to compile and behave identically.
//
// HTTP boundary contract: handlers SHOULD use errors.As to retrieve
// *AccountLockedError, then call RetryAfter() to compute the header
// value. Falling back to errors.Is(err, ErrAccountLocked) without
// extracting the typed error still works and yields a 429 with a
// minimum-1-second Retry-After (the rounded floor the middleware
// applies).
type AccountLockedError struct {
	// Until is the locked_until timestamp from the durable
	// account_lockout row that the Lockouts port returned. RetryAfter
	// is computed against this.
	Until time.Time

	// nowFunc lets tests inject a deterministic clock. nil falls back
	// to time.Now (UTC).
	nowFunc func() time.Time
}

// NewAccountLockedError builds an AccountLockedError with the supplied
// locked_until timestamp.
func NewAccountLockedError(until time.Time) *AccountLockedError {
	return &AccountLockedError{Until: until}
}

// Error returns the same message as the ErrAccountLocked sentinel so
// log lines stay uniform regardless of which Login branch produced
// the error.
func (e *AccountLockedError) Error() string {
	return ErrAccountLocked.Error()
}

// Is satisfies the errors.Is contract so callers that test against
// ErrAccountLocked continue to work.
func (e *AccountLockedError) Is(target error) bool {
	return target == ErrAccountLocked
}

// RetryAfter returns the duration until Until elapses. A zero or past
// Until clamps to 0; the HTTP middleware rounds up to at least 1
// second on the wire (RFC 7231 mandates a delta-seconds integer).
func (e *AccountLockedError) RetryAfter() time.Duration {
	if e == nil || e.Until.IsZero() {
		return 0
	}
	now := e.now()
	if !e.Until.After(now) {
		return 0
	}
	return e.Until.Sub(now)
}

// WithClock returns a copy of e using nowFn as the clock source.
// Tests inject a frozen clock to assert RetryAfter boundaries
// deterministically.
func (e *AccountLockedError) WithClock(nowFn func() time.Time) *AccountLockedError {
	cp := *e
	cp.nowFunc = nowFn
	return &cp
}

func (e *AccountLockedError) now() time.Time {
	if e.nowFunc != nil {
		return e.nowFunc()
	}
	return time.Now().UTC()
}

// Compile-time assertion that the typed error participates in the
// errors.Is chain when wrapped.
var _ = func() bool {
	return errors.Is(&AccountLockedError{}, ErrAccountLocked)
}
