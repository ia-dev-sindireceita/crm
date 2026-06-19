package mastermfa

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

// masterCSRFTokenDomain namespaces the form-token hash so the derived
// value can never collide with any other SHA-256 digest the codebase
// computes over a bare UUID. Keep stable — changing it only rotates the
// rendered form tokens, which is harmless because the token is not a
// validated secret (see CSRFTokenFromContext).
const masterCSRFTokenDomain = "crm:master-csrf-form:"

// CSRFTokenFromContext is the CSRF-token provider for the relocated
// /master/* operator surface (SIN-65289). It mirrors the tenant-side
// csrfTokenFromSessionContext helper in cmd/server, but reads the master
// operator from the master-session context (MasterFromContext) instead
// of the tenant session — the relocated surface (SIN-65264 Leg 5b) runs
// on the master-host chain
//
//	MasterHostOnly → RequireMasterOriginCSRF → RequireMasterAuth →
//	RequireMasterMFA → RequirePrincipalFromMaster → RequireAction → handler
//
// which installs NO tenant session, so the tenant provider returned ""
// and every GET /master/* that renders a form 500'd with
// "csrf token missing" in staging. masterweb handlers treat an empty
// token as a programmer error (500), so the wire MUST supply a non-empty
// token whenever the master chain authenticated the request.
//
// Security note — this token is NOT the CSRF control. SIN-65269 (SecEng
// verdict CSRF-1…7) established Origin verification (RequireMasterOriginCSRF,
// Option B) as THE CSRF control for both /m/* and the relocated /master/*
// POSTs; SameSite=Strict on __Host-sess-master is the defense-in-depth
// layer. No handler on the master chain re-validates this form token. It
// therefore only needs to be a stable, non-empty, opaque value so the
// hidden form field round-trips and the surface is double-submit-ready if
// a token control is ever added.
//
// The value is hex(SHA-256(domain + master.ID)): deterministic per
// operator (stable across a form re-render), opaque (does not leak the
// raw operator UUID into HTML), and free of any secret/key plumbing. If a
// real double-submit control is ever added to the master surface, this
// SHOULD be replaced with a per-session random token sourced from the
// master_session row (the session ID is not currently surfaced in the
// request context).
//
// Returns "" when no master is bound to the context — i.e. the surface
// was reached without the master-auth chain. That is a programmer error
// (a misconfigured mount), and surfacing it as a 500 preserves the same
// fail-closed contract the tenant provider has, rather than rendering a
// blank-token form.
func CSRFTokenFromContext(r *http.Request) string {
	master, ok := MasterFromContext(r.Context())
	if !ok {
		return ""
	}
	sum := sha256.Sum256([]byte(masterCSRFTokenDomain + master.ID.String()))
	return hex.EncodeToString(sum[:])
}
