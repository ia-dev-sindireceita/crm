// Package sessioncookie owns the on-the-wire cookie format for the IAM
// session cookies and the CSRF cookie, per ADR 0073 D2 / D1. Three
// distinct cookie buckets:
//
//   - __Host-sess-master  (master operator console; SameSite=Strict)
//   - __Host-sess-tenant  (tenant workspace; SameSite=Lax — relies on CSRF)
//   - __Host-csrf         (CSRF token; HttpOnly=false so HTMX can read it)
//
// The helpers are deliberately net/http-only — no chi, no echo. Any
// router that exposes a stdlib http.ResponseWriter / http.Request can
// use them. The IAM core (internal/iam) does not import this package;
// only the HTTP edge does.
package sessioncookie

import (
	"net/http"
)

// ADR 0073 D2 cookie names. The __Host- prefix forces Secure + Path=/ +
// no Domain attribute (i.e. host-locked). Distinct names per audience let
// an operator hold a master AND a tenant session in the same browser
// without the looser tenant SameSite leaking into the master scope.
const (
	NameMaster = "__Host-sess-master"
	NameTenant = "__Host-sess-tenant"
	NameCSRF   = "__Host-csrf"
)

// ErrCookieMissing is returned by Read when the cookie is absent or
// present-but-empty. Aliased to http.ErrNoCookie so callers can also use
// errors.Is on the stdlib sentinel.
var ErrCookieMissing = http.ErrNoCookie

// SetMaster writes the master session cookie. value is the session id;
// maxAge is the master hard timeout in seconds (ADR 0073 D3 = 4h). The
// cookie is host-locked (__Host-), Secure, HttpOnly, SameSite=Strict —
// which is non-negotiable for the operator console: any cross-site
// navigation that fires a master action is illegitimate by definition.
func SetMaster(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     NameMaster,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// SetTenant writes the tenant session cookie. SameSite=Lax is acceptable
// ONLY because every state-changing tenant route requires the CSRF token
// (ADR 0073 D1). Lax is the floor for usable UX: an atendente clicking
// a deep link from internal email opens the tenant page with the session
// attached; Strict would force a re-login.
func SetTenant(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     NameTenant,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// SetCSRF writes the CSRF cookie. HttpOnly is FALSE on purpose — HTMX
// (via the meta tag) and the form helper need to read the value to echo
// it back on writes. The token is not session material; theft of the
// token alone, without the session cookie (HttpOnly), achieves nothing.
// SameSite=Strict matches the master session because a cross-site
// request should not see the CSRF cookie at all.
func SetCSRF(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     NameCSRF,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: false,
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearMaster instructs the browser to drop the master session cookie.
// Sends an empty value with MaxAge=-1; the flags MUST match the
// originally-set cookie or some browsers ignore the deletion.
func ClearMaster(w http.ResponseWriter) {
	clearBucket(w, NameMaster, http.SameSiteStrictMode, true)
}

// ClearTenant drops the tenant session cookie.
func ClearTenant(w http.ResponseWriter) {
	clearBucket(w, NameTenant, http.SameSiteLaxMode, true)
}

// ClearCSRF drops the CSRF cookie. HttpOnly stays false to match the
// flags used on Set.
func ClearCSRF(w http.ResponseWriter) {
	clearBucket(w, NameCSRF, http.SameSiteStrictMode, false)
}

func clearBucket(w http.ResponseWriter, name string, sameSite http.SameSite, httpOnly bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: httpOnly,
		SameSite: sameSite,
	})
}

// Read returns the value of cookie name, or ErrCookieMissing if the
// cookie is absent or present-but-empty. Empty values are treated as
// missing because a present-empty cookie is the result of a Clear call —
// a stale clear should not be treated as a live session id.
func Read(r *http.Request, name string) (string, error) {
	c, err := r.Cookie(name)
	if err != nil {
		return "", err
	}
	if c.Value == "" {
		return "", ErrCookieMissing
	}
	return c.Value, nil
}
