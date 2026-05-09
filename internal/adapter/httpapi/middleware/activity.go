package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
)

// SessionToucher is the slice of iam.SessionStore the Activity
// middleware needs. Defining a narrow port here keeps the middleware
// test-friendly without dragging the full SessionStore through the
// fakes; the production *postgres.SessionStore satisfies it.
type SessionToucher interface {
	Touch(ctx context.Context, tenantID, sessionID uuid.UUID, lastActivity time.Time) error
}

// ActivityConfig is the constructor input for the Activity middleware.
//
// Sessions is the Touch port (typically the same iam.Service /
// *postgres.SessionStore instance the Auth middleware uses). Now is an
// overridable clock; tests inject a frozen clock to assert idle/hard
// boundaries deterministically. Logger is the structured log sink for
// the rare 500 paths (Touch transient failure).
type ActivityConfig struct {
	Sessions SessionToucher
	Now      func() time.Time
	Logger   *slog.Logger
}

// Activity is the per-request activity gate added in SIN-62377 to plug
// the FAIL-4 dead-code on internal/iam/timeouts.go (CheckActivity +
// TimeoutsForRole defined and unit-tested but never called from a
// production code path before this PR).
//
// Mounted in the authed group AFTER middleware.Auth (which has already
// loaded iam.Session into the context, attached the resolved tenant
// via tenancy.FromContext, and validated ExpiresAt). Activity layers
// the per-role idle/hard timeouts on top:
//
//  1. Pull the session out of the context (Auth always populates it
//     in this group; missing it is a wiring bug → 500).
//  2. Call iam.CheckActivity(role, createdAt, lastActivity, now).
//  3. On ErrSession{Idle,Hard}Timeout: clear __Host-sess-tenant and
//     redirect to /login?next=<original>. This matches the Auth
//     middleware's "session vanished" branch shape so a hostile
//     observer cannot distinguish "you were idle too long" from "you
//     never logged in".
//  4. On pass: call Sessions.Touch to bump last_activity = now() so
//     the next request starts a fresh idle window. Touch failures
//     fall through to the redirect (typical race: another tab logged
//     this user out) on ErrSessionNotFound, or 500 on transient
//     storage errors.
//
// Idle/hard windows come from internal/iam/timeouts.go's per-role
// table — RoleTenantCommon (the login default) gets Idle=30 min,
// Hard=8 h.
func Activity(cfg ActivityConfig) func(http.Handler) http.Handler {
	if cfg.Sessions == nil {
		panic("middleware: Activity Sessions is nil")
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sess, ok := SessionFromContext(r.Context())
			if !ok {
				// Wiring bug: Activity must be mounted AFTER Auth.
				logger.ErrorContext(r.Context(), "middleware: activity reached without auth session")
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			t := now()
			if err := iam.CheckActivity(sess.Role, sess.CreatedAt, sess.LastActivity, t); err != nil {
				if errors.Is(err, iam.ErrSessionIdleTimeout) || errors.Is(err, iam.ErrSessionHardTimeout) {
					sessioncookie.ClearTenant(w)
					redirectToLogin(w, r)
					return
				}
				if errors.Is(err, iam.ErrUnknownRole) {
					// Fail-closed per timeouts.go doc-comment. Treat
					// the same as an idle/hard timeout: clear cookie,
					// redirect to login. The user's next attempt will
					// land on a session row whose role is one of the
					// four valid values (login mints with
					// RoleTenantCommon by default).
					sessioncookie.ClearTenant(w)
					redirectToLogin(w, r)
					return
				}
				// Defensive — CheckActivity returns only the three
				// errors above; any new sentinel surfaces here as 500
				// rather than silently letting the request through.
				logger.ErrorContext(r.Context(), "middleware: activity check unexpected error",
					slog.String("session_id_prefix", sess.ID.String()[:8]),
					slog.String("error", err.Error()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}

			if err := cfg.Sessions.Touch(r.Context(), sess.TenantID, sess.ID, t); err != nil {
				if errors.Is(err, iam.ErrSessionNotFound) {
					sessioncookie.ClearTenant(w)
					redirectToLogin(w, r)
					return
				}
				logger.ErrorContext(r.Context(), "middleware: activity touch failed",
					slog.String("session_id_prefix", sess.ID.String()[:8]),
					slog.String("error", err.Error()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
