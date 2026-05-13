// Package loginhandler renders the POST /login endpoint and the
// shared error-translation helper that turns iam.Service.Login
// outcomes into HTTP responses (SIN-62348 §"HTTP middleware: 429 +
// Retry-After").
//
// The handler is transport-only: it pulls form fields off the
// request, forwards them to the supplied LoginFunc, and writes
// either an HTMX-friendly success fragment or one of the modeled
// error responses below.
//
// Error mapping (WriteLoginError):
//
//   - *iam.AccountLockedError → 429 Too Many Requests with
//     Retry-After (delta-seconds, rounded UP, minimum 1) computed
//     from the typed error's Until timestamp. Body is a short HTMX
//     swap fragment.
//   - errors.Is(err, iam.ErrInvalidCredentials) → 401 Unauthorized
//     with a uniform "credenciais inválidas" fragment.
//   - everything else → 500. The handler logs the underlying
//     message; the response body stays generic so internal failures
//     do not leak.
//
// The helper is exposed so any other middleware that invokes
// iam.Service.Login (e.g. master endpoints) can share the same
// translation contract.
package loginhandler

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/pericles-luz/crm/internal/iam"
)

// LoginFunc is the slice of iam.Service.Login the handler needs.
// cmd/server wires either the shared tenant Service.Login method
// directly or a per-request closure that builds a tenant-scoped
// Service first (NewTenantLockouts(pool, tenant.ID)).
//
// route is the HTTP path that handled the request (ADR 0074 §6); it
// flows into the master-lockout Slack alert. Tenant alerts ignore it.
type LoginFunc func(ctx context.Context, host, email, password string, ipAddr net.IP, userAgent, route string) (iam.Session, error)

// Option configures the handler. Reserved for future extension
// (cookie writer, success-redirect URL); keeping the signature open
// avoids a churn-y Handler signature change later.
type Option func(*config)

type config struct {
	logger *slog.Logger
}

// WithLogger overrides the default slog.Default sink.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) {
		if l != nil {
			c.logger = l
		}
	}
}

// New returns a POST-only http.Handler that calls login and renders
// the appropriate response. A nil login is a programmer error and
// panics at wire time so a misconfigured router fails loudly.
func New(login LoginFunc, opts ...Option) http.Handler {
	if login == nil {
		panic("loginhandler: login is nil")
	}
	cfg := config{logger: slog.Default()}
	for _, opt := range opts {
		opt(&cfg)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		email := r.PostFormValue("email")
		password := r.PostFormValue("password")
		ip := remoteIP(r)
		ua := r.UserAgent()

		_, err := login(r.Context(), r.Host, email, password, ip, ua, r.URL.Path)
		if err != nil {
			WriteLoginError(w, r, err, cfg.logger)
			return
		}

		// Success: minimal HTMX-friendly OK fragment. Cookie writing
		// + redirect is a follow-up ticket (master-MFA / session UX);
		// the SIN-62348 scope ends at the 429-translation contract.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<p>signed in</p>\n"))
	})
}

// WriteLoginError renders the response for an error returned by an
// iam.Service.Login call. See package-doc for the full mapping.
//
// The function is exported so master endpoints, future SSO bridges,
// and any other transport that drives Login share the same wire
// contract — keeping Retry-After arithmetic, status codes, and
// fragment bodies in one place.
func WriteLoginError(w http.ResponseWriter, r *http.Request, err error, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	var locked *iam.AccountLockedError
	if errors.As(err, &locked) {
		writeAccountLocked(w, r, locked, logger)
		return
	}
	if errors.Is(err, iam.ErrInvalidCredentials) {
		writeInvalidCredentials(w)
		return
	}
	logger.WarnContext(r.Context(), "login: internal error",
		slog.String("err", err.Error()),
	)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func writeAccountLocked(w http.ResponseWriter, r *http.Request, locked *iam.AccountLockedError, logger *slog.Logger) {
	secs := retryAfterSeconds(locked.RetryAfter())
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte("<p>conta temporariamente bloqueada</p>\n"))
	logger.InfoContext(r.Context(), "login: 429 account locked",
		slog.Int64("retry_after_s", secs),
	)
}

func writeInvalidCredentials(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte("<p>credenciais inválidas</p>\n"))
}

// retryAfterSeconds converts a duration to a delta-seconds integer
// per RFC 7231 §7.1.3, rounding UP so a sub-second remainder still
// surfaces as a non-zero header. A zero or negative input clamps to
// 1 so clients always see an actionable value.
func retryAfterSeconds(d time.Duration) int64 {
	secs := int64(math.Ceil(d.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return secs
}

// remoteIP strips the port off r.RemoteAddr and parses the host
// portion. Unparseable addresses return nil — Login accepts a nil
// IP and stamps the session row accordingly.
//
// Production reverse-proxy wiring: the trusted-proxy parsing of
// X-Forwarded-For is the responsibility of the http server bootstrap
// (Caddy, the load balancer) which writes the canonical client IP
// into r.RemoteAddr before this handler runs.
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
