package validation

import (
	"context"
	"crypto/subtle"
	"fmt"
	"strings"

	"github.com/pericles-luz/crm/internal/iam/dnsresolver"
)

// txtSubdomain is the label we publish in customer-facing instructions:
// "create a TXT record at _crm-verify.<your-host> containing <token>". It
// is a constant rather than a configurable so an instance cannot be set
// up to look up TXTs at the apex (which would let any operator who can
// publish a TXT for some other reason bypass ownership proof).
const txtSubdomain = "_crm-verify."

// Validator is the use-case for ADR 0079 §1. It is constructed once at
// boot with concrete adapters, then handed to whatever HTTP/handler layer
// needs to verify ownership.
//
// The struct holds dependencies, never per-request state. It is safe for
// concurrent use as long as the adapters it wraps are.
type Validator struct {
	resolver dnsresolver.Resolver
	auditor  Auditor
	writer   Writer
	clock    Clock
}

// Option mutates a Validator at construction time. Used to plug in
// optional collaborators (currently only Writer) without breaking the
// existing positional New signature.
type Option func(*Validator)

// WithWriter plugs a dns_resolution_log Writer into the validator.
// Validation paths emit one entry per terminal outcome (success, block,
// or error). Writer failures are logged but never bubble up — losing a
// single audit row is preferable to denying a legitimate validation.
func WithWriter(w Writer) Option {
	return func(v *Validator) {
		if w == nil {
			return
		}
		v.writer = w
	}
}

// New builds a Validator. Passing a nil Auditor is allowed and uses a
// silent fallback (see noopAuditor in ports.go); passing a nil Clock or
// nil Resolver is a programmer error and will panic on the first call,
// because the alternative — silently using time.Now and a no-op resolver —
// would silently weaken the security contract.
//
// We do not panic at construction time so this can be called from main
// before adapters are wired (e.g. in graceful-shutdown ordering tests).
//
// The variadic opts argument is the forward-compatible seam for adding
// a Writer (or future collaborators) without changing the positional
// signature: tests and existing wire-up calling New(resolver, auditor,
// clock) still compile and behave identically.
func New(resolver dnsresolver.Resolver, auditor Auditor, clock Clock, opts ...Option) *Validator {
	if auditor == nil {
		auditor = noopAuditor{}
	}
	v := &Validator{
		resolver: resolver,
		auditor:  auditor,
		writer:   noopWriter{},
		clock:    clock,
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// emit writes one LogEntry, swallowing the adapter's error. The Auditor
// already records every audit event; the Writer outage is best-effort.
// Callers MUST NOT rely on the return value for control flow — it exists
// only for cleaner unit-test assertions.
func (v *Validator) emit(ctx context.Context, e LogEntry) {
	if v.writer == nil {
		return
	}
	_ = v.writer.Write(ctx, e)
}

// Validate is the one entry point. The contract is:
//
//   - Empty host or expectedToken → ErrEmptyHost / ErrEmptyToken (programmer
//     error; audit fired with EventEmptyInput so a misconfigured caller is
//     visible in the audit log).
//   - Resolver error on either lookup → wrapped error with %w semantics;
//     EventResolverError. Caller MAY retry.
//   - Host has no A/AAAA → ErrNoAddress + EventNoAddress.
//   - ANY answer falls in a blocked CIDR → ErrPrivateIP + EventBlockedSSRF.
//     We REJECT even when only one of N answers is blocked, because a
//     mixed answer is a textbook DNS-rebinding setup.
//   - No TXT under _crm-verify.<host> matches expectedToken → ErrTokenMismatch
//   - EventTokenMismatch.
//   - Success → Result populated + EventValidatedOK.
//
// On success, Result.IP is the FIRST non-blocked IP returned by the
// resolver; callers MUST persist this in dns_resolution_log so a follow-up
// rebind from the same host is observable (ADR 0079 §2).
func (v *Validator) Validate(ctx context.Context, host, expectedToken string) (Result, error) {
	host = strings.TrimSpace(strings.ToLower(host))
	expectedToken = strings.TrimSpace(expectedToken)
	now := v.clock.Now()
	tenantID := TenantIDFromContext(ctx)

	if host == "" {
		v.auditor.Record(ctx, AuditEvent{Event: EventEmptyInput, Host: host, At: now, Detail: map[string]string{"reason": "host"}})
		v.emit(ctx, LogEntry{TenantID: tenantID, Host: host, Decision: DecisionError, Reason: ReasonEmptyInput, Phase: PhaseValidate, At: now})
		return Result{}, ErrEmptyHost
	}
	if expectedToken == "" {
		v.auditor.Record(ctx, AuditEvent{Event: EventEmptyInput, Host: host, At: now, Detail: map[string]string{"reason": "token"}})
		v.emit(ctx, LogEntry{TenantID: tenantID, Host: host, Decision: DecisionError, Reason: ReasonEmptyInput, Phase: PhaseValidate, At: now})
		return Result{}, ErrEmptyToken
	}

	answers, err := v.resolver.LookupIP(ctx, host)
	if err != nil {
		v.auditor.Record(ctx, AuditEvent{Event: EventResolverError, Host: host, At: now, Detail: map[string]string{"phase": "ip", "err": err.Error()}})
		v.emit(ctx, LogEntry{TenantID: tenantID, Host: host, Decision: DecisionError, Reason: ReasonResolverError, Phase: PhaseValidate, At: now})
		return Result{}, fmt.Errorf("validation: lookup IP for %q: %w", host, err)
	}
	if len(answers) == 0 {
		v.auditor.Record(ctx, AuditEvent{Event: EventNoAddress, Host: host, At: now})
		v.emit(ctx, LogEntry{TenantID: tenantID, Host: host, Decision: DecisionError, Reason: ReasonNoAddress, Phase: PhaseValidate, At: now})
		return Result{}, ErrNoAddress
	}

	// SSRF guard: reject if ANY answer is blocked. We intentionally do not
	// log the blocked IP itself (see Auditor doc): the host is enough for
	// the alert, and including the IP would mirror the attacker's chosen
	// address through our log pipeline.
	dnssecForPick := true
	var pick dnsresolver.IPAnswer
	picked := false
	for _, a := range answers {
		if isBlocked(a.IP) {
			v.auditor.Record(ctx, AuditEvent{Event: EventBlockedSSRF, Host: host, At: now, Detail: map[string]string{"answers": fmt.Sprintf("%d", len(answers))}})
			// PinnedIP is intentionally zero — we MUST NOT persist the
			// attacker-chosen address in the log pipeline.
			v.emit(ctx, LogEntry{TenantID: tenantID, Host: host, Decision: DecisionBlock, Reason: ReasonPrivateIP, Phase: PhaseValidate, At: now})
			return Result{}, ErrPrivateIP
		}
		// AND-style: if any answer is unsigned, the verified-with-DNSSEC
		// flag we persist drops to false. That gives anti-rebinding
		// reviewers an unambiguous signal.
		if !a.VerifiedWithDNSSEC {
			dnssecForPick = false
		}
		if !picked {
			pick = a
			picked = true
		}
	}

	// TXT proof of ownership.
	txts, err := v.resolver.LookupTXT(ctx, txtSubdomain+host)
	if err != nil {
		v.auditor.Record(ctx, AuditEvent{Event: EventResolverError, Host: host, At: now, Detail: map[string]string{"phase": "txt", "err": err.Error()}})
		v.emit(ctx, LogEntry{TenantID: tenantID, Host: host, Decision: DecisionError, Reason: ReasonResolverError, Phase: PhaseValidate, At: now})
		return Result{}, fmt.Errorf("validation: lookup TXT for %q: %w", txtSubdomain+host, err)
	}
	if !containsToken(txts, expectedToken) {
		v.auditor.Record(ctx, AuditEvent{Event: EventTokenMismatch, Host: host, At: now, Detail: map[string]string{"records": fmt.Sprintf("%d", len(txts))}})
		v.emit(ctx, LogEntry{TenantID: tenantID, Host: host, Decision: DecisionBlock, Reason: ReasonTokenMismatch, Phase: PhaseValidate, At: now})
		return Result{}, ErrTokenMismatch
	}

	res := Result{
		IP:                 pick.IP,
		VerifiedAt:         now,
		VerifiedWithDNSSEC: dnssecForPick,
	}
	v.auditor.Record(ctx, AuditEvent{
		Event: EventValidatedOK,
		Host:  host,
		At:    now,
		Detail: map[string]string{
			"ip":     res.IP.String(),
			"dnssec": fmt.Sprintf("%t", res.VerifiedWithDNSSEC),
		},
	})
	v.emit(ctx, LogEntry{
		TenantID:           tenantID,
		Host:               host,
		PinnedIP:           res.IP,
		VerifiedWithDNSSEC: res.VerifiedWithDNSSEC,
		Decision:           DecisionAllow,
		Reason:             ReasonOK,
		Phase:              PhaseValidate,
		At:                 now,
	})
	return res, nil
}

// ValidateHostOnly runs the IP-allowlist half of Validate without the TXT
// proof. It is the pre-flight the Enroll path uses: at enrollment time the
// tenant has not published the verification TXT yet, but we still want to
// reject hosts that resolve to a private/loopback range so the user is not
// asked to publish a token they will never be able to verify.
//
// Contract:
//
//   - Empty host → ErrEmptyHost + EventEmptyInput.
//   - Resolver error → wrapped error + EventResolverError.
//   - No A/AAAA → ErrNoAddress + EventNoAddress.
//   - Any answer in the blocked CIDRs → ErrPrivateIP + EventBlockedSSRF.
//   - Otherwise → nil error. The audit log records EventValidatedOK with
//     a `phase=host_only` detail so reviewers can tell pre-flight events
//     apart from end-to-end ones.
func (v *Validator) ValidateHostOnly(ctx context.Context, host string) error {
	host = strings.TrimSpace(strings.ToLower(host))
	now := v.clock.Now()
	tenantID := TenantIDFromContext(ctx)

	if host == "" {
		v.auditor.Record(ctx, AuditEvent{Event: EventEmptyInput, Host: host, At: now, Detail: map[string]string{"reason": "host", "phase": "host_only"}})
		v.emit(ctx, LogEntry{TenantID: tenantID, Host: host, Decision: DecisionError, Reason: ReasonEmptyInput, Phase: PhaseHostOnly, At: now})
		return ErrEmptyHost
	}
	answers, err := v.resolver.LookupIP(ctx, host)
	if err != nil {
		v.auditor.Record(ctx, AuditEvent{Event: EventResolverError, Host: host, At: now, Detail: map[string]string{"phase": "host_only", "err": err.Error()}})
		v.emit(ctx, LogEntry{TenantID: tenantID, Host: host, Decision: DecisionError, Reason: ReasonResolverError, Phase: PhaseHostOnly, At: now})
		return fmt.Errorf("validation: lookup IP for %q: %w", host, err)
	}
	if len(answers) == 0 {
		v.auditor.Record(ctx, AuditEvent{Event: EventNoAddress, Host: host, At: now, Detail: map[string]string{"phase": "host_only"}})
		v.emit(ctx, LogEntry{TenantID: tenantID, Host: host, Decision: DecisionError, Reason: ReasonNoAddress, Phase: PhaseHostOnly, At: now})
		return ErrNoAddress
	}
	for _, a := range answers {
		if isBlocked(a.IP) {
			v.auditor.Record(ctx, AuditEvent{Event: EventBlockedSSRF, Host: host, At: now, Detail: map[string]string{"phase": "host_only", "answers": fmt.Sprintf("%d", len(answers))}})
			v.emit(ctx, LogEntry{TenantID: tenantID, Host: host, Decision: DecisionBlock, Reason: ReasonPrivateIP, Phase: PhaseHostOnly, At: now})
			return ErrPrivateIP
		}
	}
	v.auditor.Record(ctx, AuditEvent{Event: EventValidatedOK, Host: host, At: now, Detail: map[string]string{"phase": "host_only"}})
	// Pre-flight does not pin an IP (the Verify path does). We persist
	// allow/ok with a zero PinnedIP so reviewers can still see that the
	// pre-flight passed.
	v.emit(ctx, LogEntry{TenantID: tenantID, Host: host, Decision: DecisionAllow, Reason: ReasonOK, Phase: PhaseHostOnly, At: now})
	return nil
}

// containsToken compares each TXT record to the expected token using a
// constant-time primitive. Defense in depth (AGENTS.md security lens:
// "constant-time comparison for secrets" — the per-domain verification
// token is treated as a secret in the threat model, even though it is
// burned only once at enrollment and rotated on every retry).
//
// We accept that subtle.ConstantTimeCompare returns 0 immediately when
// the lengths differ — i.e. the length of `expected` is leaked. That
// length is part of the public product spec (32 hex chars) so there is
// nothing secret to protect there.
//
// We short-circuit on the first match. The attacker controls the TXT
// side, so the position of the matching record cannot leak anything
// about the secret expected value: by the time a TXT comparison
// succeeds, the attacker already knew which slot they put it in.
func containsToken(txts []string, expected string) bool {
	want := []byte(expected)
	for _, t := range txts {
		got := []byte(strings.TrimSpace(t))
		if subtle.ConstantTimeCompare(got, want) == 1 {
			return true
		}
	}
	return false
}
