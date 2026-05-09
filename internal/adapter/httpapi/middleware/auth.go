package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// SessionCookieName is the cookie that carries the per-tenant session id.
// Per ADR 0073 §D2 the on-the-wire name is __Host-sess-tenant, which forces
// Secure + Path=/ + no Domain at the browser. The constant is an alias for
// sessioncookie.NameTenant so callers in this package keep a stable symbol
// and tests can reference it without importing two packages.
const SessionCookieName = sessioncookie.NameTenant

// SessionValidator is the slice of iam.Service the Auth middleware needs.
// Defining it as an interface here keeps the middleware test-friendly
// without dragging the rest of iam.Service through the mocks. The concrete
// *iam.Service satisfies it.
type SessionValidator interface {
	ValidateSession(ctx context.Context, tenantID, sessionID uuid.UUID) (iam.Session, error)
}

// sessionCtxKey is the unexported context-key type for the resolved
// session. Kept in this package so handlers in sibling packages must call
// SessionFromContext rather than reach for the raw key.
type sessionCtxKey struct{}

// WithSession attaches the validated session to ctx for downstream
// handlers. Tests use it to seed contexts without going through the full
// Auth chain.
func WithSession(ctx context.Context, s iam.Session) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, s)
}

// SessionFromContext returns the session injected by the Auth middleware.
// The bool is false when the request never went through Auth — callers
// SHOULD treat that as a programmer error rather than a 401 path.
func SessionFromContext(ctx context.Context) (iam.Session, bool) {
	s, ok := ctx.Value(sessionCtxKey{}).(iam.Session)
	return s, ok
}

// Auth validates the crm_session cookie against the tenant resolved by
// TenantScope and either injects the session into the context or
// redirects to /login?next=<original> (302 — not 401 — so the HTMX-server-
// rendered UX flows naturally).
//
// All credential-mismatch failure modes — missing cookie, malformed uuid,
// unknown session, expired session, cross-tenant probe — collapse to the
// same redirect so a hostile caller cannot distinguish "you have no
// session" from "your session belongs to another tenant".
func Auth(v SessionValidator) func(http.Handler) http.Handler {
	if v == nil {
		panic("middleware: Auth validator is nil")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant, err := tenancy.FromContext(r.Context())
			if err != nil {
				http.Error(w, "tenant scope missing", http.StatusInternalServerError)
				return
			}
			value, err := sessioncookie.Read(r, SessionCookieName)
			if err != nil {
				redirectToLogin(w, r)
				return
			}
			sessionID, err := uuid.Parse(value)
			if err != nil {
				redirectToLogin(w, r)
				return
			}
			sess, err := v.ValidateSession(r.Context(), tenant.ID, sessionID)
			if err != nil {
				if errors.Is(err, iam.ErrSessionNotFound) || errors.Is(err, iam.ErrSessionExpired) {
					redirectToLogin(w, r)
					return
				}
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithSession(r.Context(), sess)))
		})
	}
}

// redirectToLogin issues a 302 with `next` set to the original URL so the
// login handler can bounce the user back after a successful sign-in. Only
// the path and query are preserved — the host/scheme are inferred by the
// browser at redirect time, which keeps the redirect safe even behind a
// reverse proxy with a different external host.
func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.RequestURI()
	target := "/login?next=" + url.QueryEscape(next)
	http.Redirect(w, r, target, http.StatusFound)
}
