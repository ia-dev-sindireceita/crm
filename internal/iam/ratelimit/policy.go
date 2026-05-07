package ratelimit

import (
	"errors"
	"fmt"
	"time"
)

// Bucket is one configured (window, max) pair inside a Policy. Each
// authentication endpoint has multiple buckets — typically one keyed
// on IP and another keyed on email/session/user — so a single policy
// row carries a slice. The Name field is the key prefix the http
// middleware feeds to the RateLimiter (e.g. "login:ip", "login:email").
//
// Window MUST be positive and Max MUST be > 0. A zero Max would
// permanently lock the bucket open, which is silently wrong; New
// returns an error in that case.
type Bucket struct {
	// Name is the per-bucket key prefix. The middleware appends the
	// extracted value (IP, hashed email, session id) to form the full
	// Redis key.
	Name string
	// Window is the rolling-window length the limiter counts over.
	Window time.Duration
	// Max is the maximum number of hits allowed inside Window. The
	// (Max+1)th hit is rejected.
	Max int
}

// Lockout describes the durable-lockout policy for an endpoint. Zero
// Threshold means "no lockout" — the rate limiter still throttles per
// Bucket, but no row is written to account_lockout. A positive
// Threshold means "after N consecutive failed attempts on the same
// principal in any Bucket window, lock for Duration".
//
// AlertOnLock toggles the synchronous Slack alert defined for the
// master endpoints in ADR 0073 §D4 (acceptance criterion #3 of
// SIN-62341). Tenant endpoints leave it false.
type Lockout struct {
	Threshold   int
	Duration    time.Duration
	AlertOnLock bool
}

// Policy bundles the buckets and lockout for one endpoint. Built once
// at process start (the middleware caches a Policy per route) so the
// hot-path cost of a rate-limit check is just an interface call into
// the limiter and the lockout store, never a re-parse of config.
//
// Policies are immutable after New returns — the slice header is
// copied on construction so mutation of the input slice does not bleed
// into the registered policy.
type Policy struct {
	// Name is a stable identifier the metrics/log layer uses
	// (e.g. "login", "m_login"). Must be non-empty.
	Name    string
	Buckets []Bucket
	Lockout Lockout
}

// ErrInvalidPolicy is returned by NewPolicy when the inputs cannot be
// turned into a usable policy. Callers should panic at start-up rather
// than swallow this — a misconfigured limiter is a security regression,
// not a runtime exception.
var ErrInvalidPolicy = errors.New("ratelimit: invalid policy")

// NewPolicy validates and freezes a Policy. It rejects:
//
//   - empty Name
//   - empty Buckets
//   - any Bucket with empty Name, non-positive Window, or non-positive Max
//   - duplicate Bucket Names within the same Policy
//   - Lockout.Threshold < 0 or Lockout.Duration < 0
//   - Lockout.Threshold > 0 with Duration == 0 (would lock for ever)
//
// Returning a non-nil error wraps ErrInvalidPolicy so callers can use
// errors.Is in tests.
func NewPolicy(name string, buckets []Bucket, lockout Lockout) (Policy, error) {
	if name == "" {
		return Policy{}, fmt.Errorf("%w: name is empty", ErrInvalidPolicy)
	}
	if len(buckets) == 0 {
		return Policy{}, fmt.Errorf("%w: %q has no buckets", ErrInvalidPolicy, name)
	}
	seen := make(map[string]struct{}, len(buckets))
	frozen := make([]Bucket, 0, len(buckets))
	for i, b := range buckets {
		if b.Name == "" {
			return Policy{}, fmt.Errorf("%w: %q bucket #%d: name is empty", ErrInvalidPolicy, name, i)
		}
		if b.Window <= 0 {
			return Policy{}, fmt.Errorf("%w: %q bucket %q: window must be > 0", ErrInvalidPolicy, name, b.Name)
		}
		if b.Max <= 0 {
			return Policy{}, fmt.Errorf("%w: %q bucket %q: max must be > 0", ErrInvalidPolicy, name, b.Name)
		}
		if _, dup := seen[b.Name]; dup {
			return Policy{}, fmt.Errorf("%w: %q bucket %q: duplicate name", ErrInvalidPolicy, name, b.Name)
		}
		seen[b.Name] = struct{}{}
		frozen = append(frozen, b)
	}
	if lockout.Threshold < 0 {
		return Policy{}, fmt.Errorf("%w: %q lockout threshold must be >= 0", ErrInvalidPolicy, name)
	}
	if lockout.Duration < 0 {
		return Policy{}, fmt.Errorf("%w: %q lockout duration must be >= 0", ErrInvalidPolicy, name)
	}
	if lockout.Threshold > 0 && lockout.Duration == 0 {
		return Policy{}, fmt.Errorf("%w: %q lockout threshold > 0 requires duration > 0", ErrInvalidPolicy, name)
	}
	return Policy{Name: name, Buckets: frozen, Lockout: lockout}, nil
}

// LockoutEnabled is a convenience predicate for the middleware.
func (p Policy) LockoutEnabled() bool { return p.Lockout.Threshold > 0 && p.Lockout.Duration > 0 }

// DefaultPolicies returns the SIN-62341 / ADR 0073 §D4 default policy
// table. The HTTP middleware loads these at startup; tests can build
// their own via NewPolicy.
//
// Numbers here are the spec floor — operators tune per bucket via the
// auth.ratelimit.* config keys (ADR 0073 §D5) without a redeploy.
func DefaultPolicies() (map[string]Policy, error) {
	specs := []struct {
		name    string
		buckets []Bucket
		lockout Lockout
	}{
		{
			name: "login",
			buckets: []Bucket{
				{Name: "ip", Window: time.Minute, Max: 5},
				{Name: "email", Window: time.Hour, Max: 10},
			},
			lockout: Lockout{Threshold: 10, Duration: 15 * time.Minute},
		},
		{
			name: "2fa_verify",
			buckets: []Bucket{
				{Name: "session", Window: time.Minute, Max: 5},
				{Name: "user", Window: time.Hour, Max: 20},
			},
			// 2FA failure does not write account_lockout — instead the
			// session is invalidated after 6 failures (handled by the
			// session/2fa flow, not this package). No durable lockout.
			lockout: Lockout{},
		},
		{
			name: "password_reset",
			buckets: []Bucket{
				{Name: "ip", Window: time.Hour, Max: 3},
				{Name: "email", Window: time.Hour, Max: 3},
			},
			lockout: Lockout{},
		},
		{
			name: "m_login",
			buckets: []Bucket{
				{Name: "ip", Window: time.Minute, Max: 3},
				{Name: "email", Window: time.Hour, Max: 5},
			},
			lockout: Lockout{Threshold: 5, Duration: 30 * time.Minute, AlertOnLock: true},
		},
		{
			name: "m_2fa_verify",
			buckets: []Bucket{
				{Name: "session", Window: time.Minute, Max: 3},
				{Name: "user", Window: time.Hour, Max: 10},
			},
			// Same as the tenant 2fa case: session invalidation after 5
			// failures is enforced by the master 2fa flow. Slack alert
			// is fired by that flow, not this policy table.
			lockout: Lockout{},
		},
	}
	out := make(map[string]Policy, len(specs))
	for _, s := range specs {
		p, err := NewPolicy(s.name, s.buckets, s.lockout)
		if err != nil {
			return nil, err
		}
		out[s.name] = p
	}
	return out, nil
}
