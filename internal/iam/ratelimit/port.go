package ratelimit

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RateLimiter is the per-window counter port. Implementations track the
// number of "hits" recorded against a key inside a rolling window of
// length window and answer whether the next hit is below the max
// threshold.
//
// Allow records the hit and returns the post-record decision in a
// single call: an implementation MUST be atomic so two concurrent
// callers cannot both squeeze under the cap (ADR 0073 §D4).
//
//   - allowed = true  — caller may proceed; this hit was counted.
//   - allowed = false — caller has exceeded the cap; retryAfter is the
//     suggested wait until the oldest in-window hit ages out
//     (Retry-After header value on the 429 response). Implementations
//     SHOULD return a positive retryAfter even on the throttled path
//     so the HTTP layer always has a non-zero value to render.
//
// The caller chooses the key shape (e.g. "login:ip:1.2.3.4",
// "login:email:alice@acme.test"). The keys() helpers in the http
// middleware build them from the request; the Redis adapter prefixes
// them with a fixed namespace and TTLs the underlying structure to
// 2×window so abandoned keys self-collect.
type RateLimiter interface {
	Allow(ctx context.Context, key string, window time.Duration, max int) (allowed bool, retryAfter time.Duration, err error)
}

// Lockouts persists the "principal is locked until time T" state in
// durable storage (Postgres). The contract is deliberately tiny: the
// rate-limit middleware is the only producer (Lock on N consecutive
// failures) and the login handler is the only consumer (IsLocked
// before verifying the password). Clear is for ops + the
// successful-login codepath that wants to reset the counter.
//
// userID names the principal even for tenant-scoped lockouts; the
// adapter resolves the right transactional scope (WithTenant vs
// WithMasterOps) at construction time, so the port stays
// scope-agnostic.
//
// IsLocked returns the locked_until timestamp on a positive response
// so the HTTP layer can render a precise Retry-After. On the
// not-locked path the returned time is zero — callers MUST gate on
// the boolean before reading the timestamp.
type Lockouts interface {
	Lock(ctx context.Context, userID uuid.UUID, until time.Time, reason string) error
	IsLocked(ctx context.Context, userID uuid.UUID) (locked bool, until time.Time, err error)
	Clear(ctx context.Context, userID uuid.UUID) error
}
