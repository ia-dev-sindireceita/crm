package mastermfa

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/loginhandler"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
)

// MasterLoginFunc is the slice of iam.Service.Login the master login
// handler depends on. The signature mirrors loginhandler.LoginFunc so
// cmd/server can wire the master service factory's Login method
// directly. Returning iam.Session is intentional even though the
// master flow does not use the tenant session — the handler reads the
// UserID off the result to mint a fresh master_session row.
//
// route is the HTTP path that handled the request (ADR 0074 §6); the
// master-lockout Slack alert carries it so the on-call operator can
// correlate the event against the access log.
type MasterLoginFunc func(ctx context.Context, host, email, password string, ipAddr net.IP, userAgent, route string) (iam.Session, error)

// LoginHandlerConfig is the constructor input. All four fields are
// required; nil panics at wire time.
type LoginHandlerConfig struct {
	Login           MasterLoginFunc
	Sessions        SessionStore
	HardTTL         time.Duration
	Logger          *slog.Logger
	VerifyPath      string
	GenericErrorMsg string
}

// LoginHandler renders GET /m/login (form) and POST /m/login (submit).
//
// On a successful POST the handler:
//
//  1. Calls Login (the master iam.Service.Login). All credential-
//     mismatch outcomes collapse to a generic "credenciais inválidas"
//     re-render of the form per the anti-enumeration rule (matches
//     tenant loginhandler).
//  2. Creates a master_session row via SessionStore.Create with the
//     resolved UserID and HardTTL.
//  3. Writes __Host-sess-master via sessioncookie.SetMaster with the
//     hard-TTL max-age (ADR 0073 §D3 master hard = 4h).
//  4. Redirects 303 to VerifyPath (default /m/2fa/verify), preserving
//     the original ?next= query so the verify handler can bounce the
//     operator back to the originally-requested URL after success.
//
// *iam.AccountLockedError is rendered through loginhandler.WriteLoginError
// — the helper exists exactly so master and tenant share the
// 429 + Retry-After contract without duplication. Anything else
// (errors.Is(err, iam.ErrInvalidCredentials), wrong host, etc.)
// re-renders the form with the generic error.
//
// CSRF protection is supplied by the upstream RequireCSRF middleware
// at router wire-time (PR4); this handler does not re-check the token.
type LoginHandler struct {
	cfg  LoginHandlerConfig
	tmpl *template.Template
}

//go:embed templates/login.html
var loginTemplates embed.FS

// DefaultMasterHardTTL is the ADR 0073 §D3 master hard timeout —
// after which the session expires regardless of activity. The login
// handler writes this onto SessionStore.Create's expires_at and onto
// the cookie's max-age.
const DefaultMasterHardTTL = 4 * time.Hour

// DefaultGenericLoginError is the anti-enumeration message rendered
// for every credential-mismatch outcome (unknown email, wrong host,
// wrong password). Matches the tenant loginhandler shape.
const DefaultGenericLoginError = "credenciais inválidas"

// NewLoginHandler validates inputs and returns the handler. The
// embedded template is parsed eagerly: a parse failure means the
// binary itself is malformed (the template is //go:embed'ed from the
// source tree), so the constructor panics. nil deps similarly panic
// per project convention.
func NewLoginHandler(cfg LoginHandlerConfig) *LoginHandler {
	if cfg.Login == nil {
		panic("mastermfa: NewLoginHandler: Login is nil")
	}
	if cfg.Sessions == nil {
		panic("mastermfa: NewLoginHandler: Sessions is nil")
	}
	if cfg.HardTTL <= 0 {
		cfg.HardTTL = DefaultMasterHardTTL
	}
	if cfg.VerifyPath == "" {
		cfg.VerifyPath = "/m/2fa/verify"
	}
	if cfg.GenericErrorMsg == "" {
		cfg.GenericErrorMsg = DefaultGenericLoginError
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	tmpl, err := template.ParseFS(loginTemplates, "templates/login.html")
	if err != nil {
		panic("mastermfa: parse login template: " + err.Error())
	}
	return &LoginHandler{cfg: cfg, tmpl: tmpl}
}

// ServeHTTP implements http.Handler. GET renders the form; POST
// processes a submission. Other methods get 405.
func (h *LoginHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.renderForm(w, r, "", http.StatusOK)
	case http.MethodPost:
		h.handlePost(w, r)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *LoginHandler) handlePost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	email := r.PostFormValue("email")
	password := r.PostFormValue("password")
	ip := remoteIP(r)
	ua := r.UserAgent()

	sess, err := h.cfg.Login(r.Context(), r.Host, email, password, ip, ua, r.URL.Path)
	if err != nil {
		// Account-locked: 429 + Retry-After, rendered through the
		// shared translator so master and tenant report the same wire
		// shape. Acceptance criterion #4 explicitly forbids
		// duplicating this rendering.
		var locked *iam.AccountLockedError
		if errors.As(err, &locked) {
			loginhandler.WriteLoginError(w, r, err, h.cfg.Logger)
			return
		}
		// Every other failure (invalid credentials, wrong host, etc.)
		// re-renders the form with the generic message. Internal
		// errors are logged but never leaked to the operator.
		if !errors.Is(err, iam.ErrInvalidCredentials) {
			h.cfg.Logger.WarnContext(r.Context(), "mastermfa: login: internal error",
				slog.String("err", err.Error()),
			)
		}
		h.renderForm(w, r, h.cfg.GenericErrorMsg, http.StatusUnauthorized)
		return
	}

	masterSess, err := h.cfg.Sessions.Create(r.Context(), sess.UserID, h.cfg.HardTTL)
	if err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "mastermfa: master session create failed",
			slog.String("user_id", sess.UserID.String()),
			slog.String("err", err.Error()),
		)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	maxAge := int(h.cfg.HardTTL.Seconds())
	sessioncookie.SetMaster(w, masterSess.ID.String(), maxAge)

	// Cache headers on the redirect response: the master cookie is
	// being set right now and a misbehaving cache could otherwise
	// echo it to a different operator.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")

	target := ResolveReturn(r.URL.Query().Get("next"), h.cfg.VerifyPath)
	// URL-encode the value (matching middleware.redirectWithReturn) so
	// embedded query characters in the next path (e.g.
	// /m/users?filter=active&page=2) survive the round trip — bare
	// concat would let r.URL.Query().Get("return") on the verify side
	// silently drop everything after the first '&'.
	if target != h.cfg.VerifyPath {
		q := url.Values{}
		q.Set("return", target)
		target = h.cfg.VerifyPath + "?" + q.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)

	h.cfg.Logger.InfoContext(r.Context(), "mastermfa: login: ok",
		slog.String("user_id", sess.UserID.String()),
		slog.String("session_id_prefix", masterSess.ID.String()[:8]),
	)
}

// renderForm writes the login page with an optional error message
// and an explicit status code. Cache headers prevent caching so a
// browser Back-button after a failure hits the server again rather
// than re-submitting a stale form.
func (h *LoginHandler) renderForm(w http.ResponseWriter, r *http.Request, errMsg string, status int) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	data := loginViewData{
		ErrorMessage: errMsg,
		NextPath:     ResolveReturn(r.URL.Query().Get("next"), ""),
	}
	if err := h.tmpl.ExecuteTemplate(w, "login.html", data); err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "mastermfa: login template render failed",
			slog.String("error", err.Error()),
		)
	}
}

type loginViewData struct {
	ErrorMessage string
	NextPath     string
}

// remoteIP strips the port off r.RemoteAddr and parses the host
// portion. Identical contract to loginhandler.remoteIP — duplicated
// (rather than exported) because LoginHandler is a sibling package
// and the helper is one short function with no behavioural variance.
func remoteIP(r *http.Request) net.IP {
	addr := r.RemoteAddr
	if addr == "" {
		return nil
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return net.ParseIP(host)
	}
	return net.ParseIP(addr)
}
