package mastermfa

import "context"

// MasterAccessDeniedAuditor records an attempted-but-rejected access to
// the master operator surface — the detective half of CA #2, re-homed
// for the relocated design (SIN-65269 R2).
//
// Background: before relocation, an unauthorized principal reaching
// /master/* was denied at RequireAction, which wrote an authz_deny row.
// The relocated surface ( MasterHostOnly -> RequireMasterAuth ->
// RequireMasterMFA -> RequirePrincipalFromMaster -> RequireAction )
// rejects every unauthorized probe BEFORE RequireAction ever runs — at
// the host pin (off-host 404) or at RequireMasterAuth (missing/invalid
// __Host-sess-master). That makes "unauthorized reaches a master action"
// architecturally impossible (strictly stronger), but it would silently
// drop the audit row. This port re-homes that emission to the new
// chokepoints so a probe of the master surface — which includes the
// impersonation routes — still leaves a security-audit trail.
//
// Implementations MUST NOT record the cookie or session value; only the
// path, the reason, and the source Host are safe to persist. The
// production adapter maps this onto the existing audit_log_security sink
// (audit.SplitLogger / SecurityEventAuthzDeny) — no new taxonomy. A nil
// auditor is a no-op, so tests and health-only wireups are unaffected.
type MasterAccessDeniedAuditor interface {
	LogMasterAccessDenied(ctx context.Context, reason, path, host string) error
}

// Reasons passed to LogMasterAccessDenied. Stable strings — they flow
// into the audit row's target payload and dashboards split on them.
const (
	// MasterDeniedReasonOffHost — the request Host did not match the
	// configured master console host (MasterHostOnly 404 path).
	MasterDeniedReasonOffHost = "off_host"
	// MasterDeniedReasonNoSession — no __Host-sess-master cookie present
	// (RequireMasterAuth redirect-to-login path).
	MasterDeniedReasonNoSession = "no_session"
	// MasterDeniedReasonSessionInvalid — the cookie value did not parse
	// as a session id (RequireMasterAuth).
	MasterDeniedReasonSessionInvalid = "session_invalid"
	// MasterDeniedReasonSessionExpired — the session id did not resolve
	// to a live row (not found or expired) (RequireMasterAuth).
	MasterDeniedReasonSessionExpired = "session_expired"
)

// auditMasterAccessDenied is the shared nil-safe emit helper. The audit
// write is best-effort: a failed write MUST NOT change the rejection
// outcome (the request is already being denied), so the error is
// swallowed here and surfaced only through the adapter's own logging.
func auditMasterAccessDenied(ctx context.Context, auditor MasterAccessDeniedAuditor, reason, path, host string) {
	if auditor == nil {
		return
	}
	_ = auditor.LogMasterAccessDenied(ctx, reason, path, host)
}
