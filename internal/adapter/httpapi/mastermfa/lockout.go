package mastermfa

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
)

// SIN-62380 (CAVEAT-3 of SIN-62343) — defensive controls on
// /m/2fa/verify. The verify handler combines the existing per-bucket
// rate limit (m_2fa_verify policy: 3/min/session, 10/h/user) with a
// session-scoped failure counter. After LockoutThreshold consecutive
// invalid submissions, the handler invalidates the master session,
// clears the cookie, fires a Slack alert, and redirects to /m/login.
//
// ADR 0074 §6 / SIN-62343 security review (CAVEAT-3): the rate limit
// alone bounds online brute-force across the ~3M-code space (TOTP ±1
// step), but a stationary attacker who paces requests under the cap
// could still grind. The session-scoped 5-strike counter raises the
// floor: a hostile session evicts itself after five wrong codes
// regardless of pacing, and the operator gets a real-time Slack alert
// to investigate.

// LockoutThresholdDefault is the ADR 0074 §6 invalidate-after-N value.
// Five wrong codes within a single master session trip the lockout.
const LockoutThresholdDefault = 5

// VerifyFailureCounter is the session-scoped invalid-submission counter
// the verify handler increments on every wrong code. Implementations
// MUST be atomic (concurrent submissions from the same session ID
// MUST NOT undercount) and MUST self-collect: a counter that has not
// been incremented inside the master session hard cap (ADR 0073 §D3 —
// 4h) MUST disappear so a long-idle session does not inherit a stale
// strike count.
//
//   - Increment records one wrong-code attempt and returns the new
//     count. The first call for a given session MUST start at 1.
//   - Reset clears the counter. The verify handler calls this on a
//     successful submission so a flaky thumb does not accumulate
//     toward the lockout. A missing counter MUST NOT be an error —
//     the post-condition (no counter for this id) is satisfied either
//     way.
//
// The Redis adapter (internal/adapter/ratelimit/redis) is the
// production binding; tests in this package and in PR2 inject a fake
// satisfying the same contract.
type VerifyFailureCounter interface {
	Increment(ctx context.Context, sessionID uuid.UUID) (int, error)
	Reset(ctx context.Context, sessionID uuid.UUID) error
}

// MasterSessionInvalidator is the slice of master-session storage the
// verify handler invokes when the failure counter trips. It deletes
// the underlying session row (so subsequent requests on the same
// cookie are denied at the auth gate) and clears the
// __Host-sess-master cookie on the response. A missing / unparseable
// cookie is NOT an error — the post-condition (no live session row
// for this id, no cookie on the client) is satisfied either way.
//
// The HTTPSession adapter implements this alongside MasterSessionMFA
// and MasterSessionRotator so a single struct owns all cookie / row
// IO for the master session.
type MasterSessionInvalidator interface {
	Invalidate(w http.ResponseWriter, r *http.Request) error
}

// LockoutAlerter is the narrow Slack-side port the verify handler
// invokes when LockoutThreshold is hit. Pulling the port out of the
// existing mfa.Alerter (recovery-used / regenerated) keeps the
// recovery and verify-lockout alert paths independent: a lockout
// can ship without churning every existing fakeAlerter implementation.
//
// Production wires the slack.VerifyLockoutAlerter adapter, which
// reuses the existing Notifier (webhook URL, timeout, HTTP client).
// Tests in this package inject a recording fake.
type LockoutAlerter interface {
	AlertVerifyLockout(ctx context.Context, details VerifyLockoutDetails) error
}

// VerifyLockoutDetails carries the operational context an on-call
// operator needs to triage a verify-lockout alert without round-
// tripping to the audit log: actor (UserID), session id (so the
// Postgres row can be matched against master_session for IP / UA at
// login time), the trip count (always == LockoutThreshold today, but
// the field stays explicit so the threshold can be tuned without
// re-cutting the alert format), plus IP / UserAgent / Route from the
// inbound request.
type VerifyLockoutDetails struct {
	UserID    uuid.UUID
	SessionID uuid.UUID
	Failures  int
	IP        string
	UserAgent string
	Route     string
}

// SessionIDExtractor is the rate-limit key extractor that reads the
// master session id from the __Host-sess-master cookie. Used as the
// "session" bucket extractor for the m_2fa_verify policy. Returns
// "" when the cookie is missing or unreadable so the rate-limit
// middleware skips the bucket rather than 429-ing on a missing
// extractor (consistent with FormFieldExtractor / IPKeyExtractor).
//
// The extractor returns the raw cookie value, NOT the parsed uuid
// string, because the rate-limit middleware only uses it as an
// opaque key — the cookie value IS already the canonical
// per-session identifier (the same string the master-auth middleware
// parses on every request).
func SessionIDExtractor(r *http.Request) string {
	if r == nil {
		return ""
	}
	raw, err := sessioncookie.Read(r, sessioncookie.NameMaster)
	if err != nil {
		return ""
	}
	return raw
}

// MasterUserIDExtractor is the rate-limit key extractor that reads the
// authenticated master user id from the request context. Used as the
// "user" bucket extractor for the m_2fa_verify policy. Returns "" when
// the context has no Master so the rate-limit middleware skips the
// bucket — production runs this only after RequireMasterAuth has
// already populated the context, but the missing-context branch is
// defensive against test wiring that skips the auth middleware.
func MasterUserIDExtractor(r *http.Request) string {
	if r == nil {
		return ""
	}
	master, ok := MasterFromContext(r.Context())
	if !ok {
		return ""
	}
	return master.ID.String()
}
