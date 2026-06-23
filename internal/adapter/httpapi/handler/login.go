package handler

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/loginhandler"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/views"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// loginViewData is the data shape the views.Login template renders
// against on every /login pass (GET and credential-failure POST). It
// is private to this package — the template reads fields by name via
// the views.go reflection helpers (loginTenantName / loginTenantLogo /
// loginWhiteLabel) so a future caller adding fields here does not need
// to touch the template.
//
// Tenant identity (Name, Logo, WhiteLabel) is plumbed from
// tenancy.FromContext so a B2B operator hitting acme.crm sees the
// tenant brand before authenticating; SanitizeNext-clamped Next plus
// the iam-minted CSRFToken cover the credential-failure POST round
// trip. TenantLogo / WhiteLabel currently fall back to zero values —
// the tenant-settings read-port that fills them in lives in a
// follow-up issue, and the template renders gracefully when both are
// empty (word-mark fallback + "Powered by CRM Pitho" footer).
type loginViewData struct {
	Next             string
	Error            string
	CSRFToken        string
	TenantThemeStyle template.CSS
	CSPNonce         string
	TenantName       string
	TenantLogo       string
	WhiteLabel       bool
}

// buildLoginViewData composes the loginViewData for the current
// request. The pre-auth context already carries the tenant (the
// middleware.TenantScope link in the defence chain runs ABOVE /login),
// so a missing tenant means a misrouted request and we fall back to
// empty branding rather than panicking. branding.ThemeStyleFromContext
// returns the platform default palette on miss, so the inline
// <style id="tenant-theme"> always lands with readable contrast.
//
// SIN-63963 / UX-F4 — when reader is non-nil and the tenant resolves,
// TenantLogo + WhiteLabel are filled from tenant-settings storage. A nil
// reader (router tests, bootstrap deploys that haven't wired the port)
// or any read failure degrades silently to the platform word-mark +
// footer: the surface must never 500 on a branding lookup.
func buildLoginViewData(r *http.Request, next, errMsg string, reader tenancy.BrandingReader) loginViewData {
	d := loginViewData{
		Next:             next,
		Error:            errMsg,
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
	}
	t, err := tenancy.FromContext(r.Context())
	if err != nil || t == nil {
		return d
	}
	d.TenantName = t.Name
	if reader == nil {
		return d
	}
	b, err := reader.LoadBranding(r.Context(), t.ID)
	if err != nil {
		// ErrTenantNotFound is expected for a tenant with no row yet;
		// anything else is a real storage fault worth surfacing. Either
		// way we keep the word-mark fallback rather than failing the page.
		if !errors.Is(err, tenancy.ErrTenantNotFound) {
			slog.WarnContext(r.Context(), "handler: load login branding failed",
				slog.String("tenant_id", t.ID.String()),
				slog.String("err", err.Error()),
			)
		}
		return d
	}
	d.TenantLogo = b.LogoURL
	d.WhiteLabel = b.WhiteLabel
	return d
}

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
	// Branding is the optional tenant-settings read port (SIN-63963 /
	// UX-F4). When non-nil the GET /login and credential-failure renders
	// fill TenantLogo + WhiteLabel from storage; nil keeps the legacy
	// word-mark + platform-footer fallback so router tests and deploys
	// that haven't wired the port behave exactly as before.
	Branding tenancy.BrandingReader
}

// LoginGet renders the GET /login form with no tenant-settings branding
// reader wired — the word-mark + platform-footer fallback. Retained as a
// bare http.HandlerFunc so callers (and tests) that do not need the
// white-label read can mount it directly. Production wires the branded
// variant via LoginGetHandler.
func LoginGet(w http.ResponseWriter, r *http.Request) {
	renderLoginPage(w, r, SanitizeNext(r.URL.Query().Get("next")), "", nil)
}

// LoginGetHandler returns a GET /login handler bound to cfg so the
// production router can supply the tenant-settings BrandingReader. A nil
// cfg.Branding behaves identically to the bare LoginGet.
func LoginGetHandler(cfg LoginConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderLoginPage(w, r, SanitizeNext(r.URL.Query().Get("next")), "", cfg.Branding)
	}
}

// renderLoginPage is the shared GET-render path: it composes the view
// data (optionally branded via reader) and writes the 200 response. The
// credential-failure 401 path uses renderLoginError, which sets its own
// status before delegating the body here.
func renderLoginPage(w http.ResponseWriter, r *http.Request, next, errMsg string, reader tenancy.BrandingReader) {
	data := buildLoginViewData(r, next, errMsg, reader)
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
				renderLoginError(w, r, next, cfg.Branding)
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

func renderLoginError(w http.ResponseWriter, r *http.Request, next string, reader tenancy.BrandingReader) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	data := buildLoginViewData(r, next, "Email ou senha inválidos.", reader)
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
