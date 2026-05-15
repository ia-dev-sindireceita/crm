package authz

import (
	"context"
	"time"

	"github.com/pericles-luz/crm/internal/iam"
)

// Recorder persists an Authorizer Decision. The production
// implementation (AuditRecorder) writes a row into audit_log_security
// and increments Prometheus counters; tests substitute a fake.
//
// Record is called after the inner Authorizer has produced d, so impls
// MUST NOT mutate d — the Decision has already been returned to the
// caller. Record runs synchronously in the request path; impls SHOULD
// keep work cheap (a single DB INSERT + counter Inc is the budget).
type Recorder interface {
	Record(ctx context.Context, p iam.Principal, action iam.Action, r iam.Resource, d iam.Decision, now time.Time)
}

// Sampler decides whether an allow Decision is persisted to
// audit_log_security. Deny decisions are always persisted; the Sampler
// is consulted on allow only.
//
// A nil Sampler is treated as "never sample" so misconfigured wireup
// degrades to deny-only retention (still satisfies the 100%-deny half
// of ADR 0004 §6).
type Sampler interface {
	ShouldSampleAllow(ctx context.Context, p iam.Principal, action iam.Action) bool
}

// AuditingAuthorizer decorates an iam.Authorizer so every Decision
// flows through a Recorder. The wrapped Decision is returned unchanged.
type AuditingAuthorizer struct {
	inner    iam.Authorizer
	recorder Recorder
	sampler  Sampler
	now      func() time.Time
}

// Config parameterises NewAuditingAuthorizer. Inner and Recorder are
// required; Sampler may be nil. Now defaults to time.Now().UTC() —
// tests pin it for deterministic OccurredAt fields.
type Config struct {
	Inner    iam.Authorizer
	Recorder Recorder
	Sampler  Sampler
	Now      func() time.Time
}

// New returns a production-default AuditingAuthorizer. Required
// dependencies are validated at construction so wireup mistakes fail
// loudly at boot rather than silently degrading audit coverage.
func New(cfg Config) *AuditingAuthorizer {
	if cfg.Inner == nil {
		panic("authz: AuditingAuthorizer Inner is nil")
	}
	if cfg.Recorder == nil {
		panic("authz: AuditingAuthorizer Recorder is nil")
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &AuditingAuthorizer{
		inner:    cfg.Inner,
		recorder: cfg.Recorder,
		sampler:  cfg.Sampler,
		now:      now,
	}
}

// Can defers to the wrapped Authorizer and then asks the Recorder to
// persist the Decision according to the deny-100% / allow-sampled
// policy. The Decision returned to the caller is byte-identical to the
// inner Authorizer's verdict.
func (a *AuditingAuthorizer) Can(ctx context.Context, p iam.Principal, action iam.Action, r iam.Resource) iam.Decision {
	d := a.inner.Can(ctx, p, action, r)
	if a.shouldRecord(ctx, p, action, d) {
		a.recorder.Record(ctx, p, action, r, d, a.now())
	}
	return d
}

// shouldRecord encapsulates the deny-100% / allow-sampled policy. It
// is split out so tests can exercise the rule in isolation from the
// Recorder side-effect.
func (a *AuditingAuthorizer) shouldRecord(ctx context.Context, p iam.Principal, action iam.Action, d iam.Decision) bool {
	if !d.Allow {
		return true
	}
	if a.sampler == nil {
		return false
	}
	return a.sampler.ShouldSampleAllow(ctx, p, action)
}
