// Package csrf is the HTTP adapter that wires the pure-domain
// internal/iam/csrf verifier to net/http per ADR 0073 D1. It exposes:
//
//   - RequireCSRF middleware: enforces the cookie + presented + session
//     triple match plus the Origin/Referer allowlist on every
//     state-changing request (POST/PATCH/PUT/DELETE).
//   - Templ-style helpers (helpers.go): hidden form input, <meta> tag,
//     hx-headers attribute. Use html/template (no templ dep yet).
//
// The middleware does NOT import internal/iam/csrf transitively at the
// HTTP boundary by accident: it explicitly delegates the constant-time
// compare to that package, keeping the rule in one place.
package csrf

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	domaincsrf "github.com/pericles-luz/crm/internal/iam/csrf"
)

// Reason names a specific failure branch in the RequireCSRF middleware.
// ADR 0073 D1 enumerates these; surfacing them via OnReject (not the
// response body) lets metrics/log dashboards split rejection causes
// without giving the client useful side-channel data.
type Reason string

// Reason values. Keep stable — these flow into Prometheus labels.
const (
	ReasonCookieMissing        Reason = "csrf.cookie_missing"
	ReasonTokenMissing         Reason = "csrf.token_missing"
	ReasonTokenMismatch        Reason = "csrf.token_mismatch"
	ReasonSessionTokenMissing  Reason = "csrf.session_token_missing"
	ReasonSessionTokenMismatch Reason = "csrf.session_token_mismatch"
	ReasonOriginMissing        Reason = "csrf.origin_missing"
	ReasonOriginMismatch       Reason = "csrf.origin_mismatch"
	ReasonSessionLookup        Reason = "csrf.session_lookup_error"
)

// HeaderName is the X-CSRF-Token request header name. Exported so the
// templ helper and any hand-rolled HTMX caller can reference one
// constant.
const HeaderName = "X-CSRF-Token"

// FormField is the hidden form input name. Exported so the helper and
// any hand-written form share the same string.
const FormField = "_csrf"

// Config bundles the dependencies the middleware needs. Every function
// is small and side-effect-free so callers can compose them in tests
// without dragging chi or session storage in.
type Config struct {
	// SessionToken returns session.csrf_token for the request, or an
	// error if no session is bound. The middleware forwards an error
	// here as a 403 with ReasonSessionLookup — the upstream auth
	// middleware should already have attached the session, so a missing
	// session at this layer is a real configuration bug, not a 401.
	SessionToken func(*http.Request) (string, error)

	// AllowedHosts returns the Origin/Referer host allowlist for this
	// request. ADR 0073 D1 says: master host + the resolved tenant host.
	// The closure receives the live request so it can read the resolved
	// tenant from context (e.g. via tenancy.FromContext).
	AllowedHosts func(*http.Request) []string

	// Skip lets the caller exempt specific routes (HMAC-authed webhooks
	// per ADR 0073 D1 step 2). nil means never skip. The function
	// receives the live request, so route, method, and headers are all
	// available for the decision.
	Skip func(*http.Request) bool

	// OnReject is an optional hook fired with the request and the
	// rejection reason BEFORE the 403 is written. Use it to log /
	// increment a Prometheus counter without coupling the middleware to
	// slog or prometheus directly. nil = no-op.
	OnReject func(*http.Request, Reason)
}

// New returns the RequireCSRF middleware. The check chain matches ADR
// 0073 D1: safe-method passthrough → skip-list → cookie present → token
// present → cookie==presented → session==presented → Origin/Referer in
// allowlist. Any failure writes 403 "Forbidden" with no body details
// and fires OnReject.
//
// Panics on a nil SessionToken or nil AllowedHosts because both are
// load-bearing — failing closed at wire time is the right answer for a
// half-configured CSRF middleware.
func New(cfg Config) func(http.Handler) http.Handler {
	if cfg.SessionToken == nil {
		panic("csrf: SessionToken is nil")
	}
	if cfg.AllowedHosts == nil {
		panic("csrf: AllowedHosts is nil")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSafeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			if cfg.Skip != nil && cfg.Skip(r) {
				next.ServeHTTP(w, r)
				return
			}

			cookieToken, err := sessioncookie.Read(r, sessioncookie.NameCSRF)
			if err != nil {
				reject(w, r, cfg, ReasonCookieMissing)
				return
			}

			presented := r.Header.Get(HeaderName)
			formValue := ""
			if presented == "" {
				// FormValue parses the body for x-www-form-urlencoded
				// requests; safe to call here because the middleware
				// owns the request before any handler reads the body.
				formValue = r.FormValue(FormField)
			}

			if presented == "" && formValue == "" {
				reject(w, r, cfg, ReasonTokenMissing)
				return
			}
			candidate := presented
			if candidate == "" {
				candidate = formValue
			}

			// Cookie vs presented. Constant-time compare guards the
			// subdomain-takeover scenario in ADR 0073 D1 step 5.
			if subtle.ConstantTimeCompare([]byte(cookieToken), []byte(candidate)) != 1 {
				reject(w, r, cfg, ReasonTokenMismatch)
				return
			}

			// Session vs presented (delegated to the domain verifier
			// so the rule lives in exactly one place).
			sessionToken, err := cfg.SessionToken(r)
			if err != nil {
				reject(w, r, cfg, ReasonSessionLookup)
				return
			}
			switch err := domaincsrf.Verify(sessionToken, presented, formValue); err {
			case nil:
				// fall through to Origin check
			case domaincsrf.ErrTokenMissing:
				reject(w, r, cfg, ReasonTokenMissing)
				return
			case domaincsrf.ErrSessionTokenMissing:
				reject(w, r, cfg, ReasonSessionTokenMissing)
				return
			case domaincsrf.ErrTokenMismatch:
				reject(w, r, cfg, ReasonSessionTokenMismatch)
				return
			default:
				reject(w, r, cfg, ReasonSessionLookup)
				return
			}

			// Origin / Referer allowlist (independent layer).
			originHost, originReason := readOriginOrReferer(r)
			if originReason != "" {
				reject(w, r, cfg, originReason)
				return
			}
			if !hostAllowed(originHost, cfg.AllowedHosts(r)) {
				reject(w, r, cfg, ReasonOriginMismatch)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// isSafeMethod reports whether m is one of the read-only HTTP methods
// (GET, HEAD, OPTIONS) that bypass CSRF per the W3C "safe methods"
// definition and ADR 0073 D1 step 1.
func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// readOriginOrReferer returns the host portion of Origin (preferred) or
// Referer (fallback). Returns ReasonOriginMissing when both are absent.
// A malformed URL returns ReasonOriginMismatch — an unparseable Origin
// MUST NOT be treated as "no origin" and silently allowed.
func readOriginOrReferer(r *http.Request) (string, Reason) {
	if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			return "", ReasonOriginMismatch
		}
		return u.Host, ""
	}
	if referer := r.Header.Get("Referer"); referer != "" {
		u, err := url.Parse(referer)
		if err != nil || u.Host == "" {
			return "", ReasonOriginMismatch
		}
		return u.Host, ""
	}
	return "", ReasonOriginMissing
}

// hostAllowed reports whether candidate matches any host in allowed,
// case-insensitive. The allowed list is small (master + tenant), so the
// linear scan is cheaper than a map allocation per request.
func hostAllowed(candidate string, allowed []string) bool {
	for _, h := range allowed {
		if strings.EqualFold(candidate, h) {
			return true
		}
	}
	return false
}

func reject(w http.ResponseWriter, r *http.Request, cfg Config, reason Reason) {
	if cfg.OnReject != nil {
		cfg.OnReject(r, reason)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte("Forbidden\n"))
}
