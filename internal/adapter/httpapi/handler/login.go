package handler

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/loginhandler"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/views"
	"github.com/pericles-luz/crm/internal/iam"
)

// LoginAuthenticator is the slice of iam.Service the login handler needs.
// Keeping it narrow lets tests inject a fake without dragging the full
// Service surface. *iam.Service satisfies it. The route parameter is
// the HTTP path that handled the request (ADR 0074 §6) — it flows into
// the master-lockout alert so the on-call operator can correlate the
// event against the access log.
type LoginAuthenticator interface {
	Login(ctx context.Context, host, email, password string, ipAddr net.IP, userAgent, route string) (iam.Session, error)
}

// LoginConfig captures the bootstrap-time wiring the login handler needs.
// The session cookie is always written via sessioncookie.SetTenant, which
// hard-codes the ADR 0073 §D2 contract (__Host-sess-tenant; Secure;
// HttpOnly; SameSite=Lax; Path=/). The Secure attribute is non-negotiable
// — there is deliberately no env override.
//
// This handler decides the body-form interop pattern (Gate G1 of
// SIN-62217): we use application/x-www-form-urlencoded end-to-end and rely
// on r.PostFormValue, which calls r.ParseForm under the hood. ParseForm is
// idempotent because it caches the parsed values on r.PostForm — so a
// future RateLimit middleware that pre-reads r.PostFormValue("email")
// does NOT break the handler with EOF. Do NOT mix this pattern with
// json.Decode(r.Body): if a future endpoint needs JSON, bracket the
// middleware with a buffer-and-restore reader instead. See
// internal/http/middleware/ratelimit/FormFieldKey for the upstream gotcha.
type LoginConfig struct {
	IAM LoginAuthenticator
}

// LoginGet renders the GET /login form. The optional `next` query param is
// preserved on the form so a successful POST bounces the user back to the
// originally-requested URL.
func LoginGet(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Next      string
		Error     string
		CSRFToken string
	}{
		Next: SanitizeNext(r.URL.Query().Get("next")),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.Login.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
}

// LoginPost validates email/password against iam.Service.Login and, on
// success, sets the session cookie + redirects to `next` (default
// /hello-tenant). On failure, re-renders the form with a deliberately
// generic error message — the same string for every credential-mismatch
// branch (unknown email vs wrong password) so an attacker cannot
// distinguish them.
func LoginPost(cfg LoginConfig) http.HandlerFunc {
	if cfg.IAM == nil {
		panic("handler: LoginPost iam authenticator is nil")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// PostFormValue is intentional: it triggers ParseForm, which
		// caches values on r so a pre-reading middleware does not
		// leave the handler with EOF. See LoginConfig docstring.
		email := strings.TrimSpace(r.PostFormValue("email"))
		password := r.PostFormValue("password")
		next := SanitizeNext(r.PostFormValue("next"))

		ipAddr := parseRemoteIP(r.RemoteAddr)
		sess, err := cfg.IAM.Login(r.Context(), r.Host, email, password, ipAddr, r.UserAgent(), r.URL.Path)
		if err != nil {
			if errors.Is(err, iam.ErrInvalidCredentials) {
				renderLoginError(w, next)
				return
			}
			// *iam.AccountLockedError → 429 + Retry-After; any other
			// non-credential error → 500. Both go through the SIN-62348
			// translator so the lockout response surface stays in one
			// place (Retry-After header, fragment body).
			loginhandler.WriteLoginError(w, r, err, slog.Default())
			return
		}
		// MaxAge=0 keeps the cookie a session cookie (cleared on
		// browser close); the server-side iam.Session row carries the
		// authoritative TTL. Production MUST be served behind TLS so the
		// __Host- + Secure flags from sessioncookie.SetTenant are
		// honoured by the browser.
		sessioncookie.SetTenant(w, sess.ID.String(), 0)
		// ADR 0073 §D1 — mirror the per-session CSRF token into the
		// __Host-csrf cookie so the browser-side templ helpers (HTMX
		// hx-headers, hidden form input, <meta>) can echo it back on
		// every state-changing request. HttpOnly is FALSE on this
		// cookie by design — see sessioncookie.SetCSRF docstring.
		// Skipped silently when the IAM service did not mint a token
		// (e.g. legacy session row pre-dating migration 0011); the
		// CSRF middleware will reject the next write attempt with
		// csrf.cookie_missing rather than authenticating without
		// double-submit protection.
		if sess.CSRFToken != "" {
			sessioncookie.SetCSRF(w, sess.CSRFToken, 0)
		}
		http.Redirect(w, r, next, http.StatusFound)
	}
}

func renderLoginError(w http.ResponseWriter, next string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	data := struct {
		Next      string
		Error     string
		CSRFToken string
	}{
		Next:  next,
		Error: "Email ou senha inválidos.",
	}
	_ = views.Login.ExecuteTemplate(w, "layout", data)
}

// SanitizeNext clamps the post-login redirect to a same-origin path so a
// hostile caller cannot use ?next=https://attacker.example/ to trick us
// into emitting a Location header that fingerprints SSO redirects to a
// third party. Exported so tests can assert the policy directly.
func SanitizeNext(raw string) string {
	const fallback = "/hello-tenant"
	if raw == "" {
		return fallback
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fallback
	}
	if u.IsAbs() || u.Host != "" {
		return fallback
	}
	if !strings.HasPrefix(u.Path, "/") {
		return fallback
	}
	return u.RequestURI()
}

// parseRemoteIP best-effort extracts the client IP from r.RemoteAddr. The
// caller (cmd/server) is expected to have wrapped the chain with chi's
// RealIP middleware so this sees the policy-correct address; in that case
// RemoteAddr is the literal string set by RealIP. Returns nil on an
// unparseable input rather than panicking.
func parseRemoteIP(remote string) net.IP {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip
	}
	return nil
}
