package mastermfa

import (
	"log/slog"
	"net/http"
	"net/url"
)

// OriginCSRFReason names a specific rejection branch in the
// RequireMasterOriginCSRF middleware. Surfacing the cause via OnReject
// (not the response body) lets metrics/log dashboards split rejection
// causes without giving a caller a useful side-channel. Keep stable —
// these flow into Prometheus labels.
type OriginCSRFReason string

// OriginCSRFReason values.
const (
	// OriginCSRFReasonHostUnset fires when MasterHost is empty. The
	// master surface should not be mounted in that case (SecEng C3),
	// but if it is otherwise reachable the gate fails closed rather
	// than matching any origin.
	OriginCSRFReasonHostUnset OriginCSRFReason = "master_csrf.host_unset"
	// OriginCSRFReasonMissing fires when neither Origin nor Referer is
	// present on an unsafe request (SecEng C1, fail closed — a browser
	// that omits both on a state-changing POST is rejected by design).
	OriginCSRFReasonMissing OriginCSRFReason = "master_csrf.missing"
	// OriginCSRFReasonUnparsable fires when the presented Origin/Referer
	// cannot be parsed or has no host. An unparseable header MUST NOT be
	// treated as "no origin" and silently allowed.
	OriginCSRFReasonUnparsable OriginCSRFReason = "master_csrf.unparsable"
	// OriginCSRFReasonSchemeNotHTTPS fires when the presented origin is
	// not https. SecEng C2: the canonical master origin is https-only.
	OriginCSRFReasonSchemeNotHTTPS OriginCSRFReason = "master_csrf.scheme_not_https"
	// OriginCSRFReasonMismatch fires when the presented origin host does
	// not equal the single canonical master host. SecEng C2/C4: exact
	// singleton equality, never a substring/prefix or multi-entry list.
	OriginCSRFReasonMismatch OriginCSRFReason = "master_csrf.mismatch"
)

// RequireMasterOriginCSRFConfig is the constructor input for the master
// operator surface CSRF gate.
type RequireMasterOriginCSRFConfig struct {
	// MasterHost is the operator-console host (e.g. "master.crm.local").
	// The single canonical master origin is "https://" + the normalized
	// host. Empty fails closed (SecEng C3) — never match-any.
	MasterHost string

	// Logger receives a structured warning on every rejection. nil →
	// slog.Default().
	Logger *slog.Logger

	// OnReject is an optional hook fired with the request and the
	// rejection reason BEFORE the 403 is written. Use it to increment a
	// Prometheus counter without coupling the middleware to a metric
	// library. nil = no-op.
	OnReject func(*http.Request, OriginCSRFReason)
}

// RequireMasterOriginCSRF is the Option-B CSRF control for the relocated
// master operator surface and the /m/* master-auth POSTs (SIN-65269,
// SecEng verdict CSRF-1…7). It rejects every state-changing request whose
// Origin (or, when Origin is absent, Referer) does not exactly match the
// single canonical master origin = "https://" + normalized MasterHost.
//
// Why Origin verification (not SameSite alone): the master host and the
// tenant hosts share a registrable domain (subdomain tenancy), so a
// forged POST from a tenant page is *same-site* with the master host and
// therefore carries __Host-sess-master. SameSite=Strict does NOT isolate
// them. A browser victim who holds the master cookie sends an honest
// Origin = the tenant page's origin ≠ the master origin → rejected. An
// attacker who could spoof Origin is not a browser victim and so does not
// possess the cookie. Origin verification is the primary CSRF control;
// SameSite=Strict on __Host-sess-master stays as defense-in-depth (C5).
//
// Behaviour (complete mediation, SecEng C1):
//   - Safe methods (GET/HEAD/OPTIONS) pass through untouched.
//   - Unsafe methods (POST/PUT/PATCH/DELETE) MUST present an Origin or
//     Referer that parses, is https, and whose host equals the canonical
//     master host (normalized: lower/trim/strip-port, SecEng C2). Any
//     other outcome — missing both headers, unparseable, non-https,
//     host mismatch, or unset MasterHost — is a 403 with no mutation.
//
// Compose it INSIDE the MasterHostOnly host pin and the master
// auth/MFA/principal chain; it is the last gate before the per-action
// authorization on the operator routes, and a standalone gate on the
// /m/* auth POSTs (login/enroll/verify).
func RequireMasterOriginCSRF(cfg RequireMasterOriginCSRFConfig) func(http.Handler) http.Handler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	want := normalizeHost(cfg.MasterHost)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isOriginCSRFSafeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			if reason := verifyMasterOrigin(r, want); reason != "" {
				if cfg.OnReject != nil {
					cfg.OnReject(r, reason)
				}
				logger.WarnContext(r.Context(), "mastermfa: origin CSRF rejection",
					slog.String("event", "master_csrf_rejected"),
					slog.String("reason", string(reason)),
					slog.String("method", r.Method),
					slog.String("route", r.URL.Path),
				)
				forbidMasterCSRF(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isOriginCSRFSafeMethod reports whether m is a read-only HTTP method
// (GET, HEAD, OPTIONS) that bypasses the Origin gate per the W3C "safe
// methods" definition. Every other method (POST/PUT/PATCH/DELETE and any
// non-standard verb) is treated as unsafe and gated — fail closed.
func isOriginCSRFSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// verifyMasterOrigin returns "" when the request's Origin (preferred) or
// Referer (fallback) is the canonical master origin, or a non-empty
// reason otherwise. want is the already-normalized master host; an empty
// want fails closed.
func verifyMasterOrigin(r *http.Request, want string) OriginCSRFReason {
	if want == "" {
		return OriginCSRFReasonHostUnset
	}
	// Origin is preferred. "null" (a sandboxed/opaque origin) is treated
	// as absent so it falls through to Referer, then to the fail-closed
	// missing branch — it never matches the canonical origin.
	if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
		return checkMasterOriginURL(origin, want)
	}
	// Referer fallback is consulted ONLY when Origin is absent (SecEng
	// C4). A present-but-mismatched Referer is rejected by the same
	// equality rule below.
	if referer := r.Header.Get("Referer"); referer != "" {
		return checkMasterOriginURL(referer, want)
	}
	return OriginCSRFReasonMissing
}

// checkMasterOriginURL parses raw and compares its origin component to
// the canonical master origin: scheme MUST be https and the normalized
// host MUST equal want exactly (no substring/prefix — this blocks
// "https://master.crm.local.evil.com", whose host normalizes to
// "master.crm.local.evil.com" ≠ "master.crm.local").
func checkMasterOriginURL(raw, want string) OriginCSRFReason {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return OriginCSRFReasonUnparsable
	}
	if u.Scheme != "https" {
		return OriginCSRFReasonSchemeNotHTTPS
	}
	if normalizeHost(u.Host) != want {
		return OriginCSRFReasonMismatch
	}
	return ""
}

// forbidMasterCSRF writes a bare 403 with no body details — the rejection
// cause flows through OnReject/the log, never to the client.
func forbidMasterCSRF(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte("Forbidden\n"))
}
