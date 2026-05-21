package handler

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// LogoutDeleter is the slice of iam.Service the logout handler needs.
// *iam.Service satisfies it.
type LogoutDeleter interface {
	Logout(ctx context.Context, tenantID, sessionID uuid.UUID) error
}

// LogoutOption configures optional behaviour on the Logout handler.
// Existing callers (handler.Logout(svc)) work unchanged; security-aware
// wirings opt in via the With* helpers (SIN-63188 / Fase 6 PR6).
type LogoutOption func(*logoutOpts)

type logoutOpts struct {
	audit  audit.SplitLogger
	logger *slog.Logger
}

// WithLogoutAudit installs a SplitLogger that receives a
// SecurityEventLogout row after the server-side session is deleted.
// The audit is best-effort: a write failure is logged but does NOT
// prevent the cookie clear or the redirect — once the user has clicked
// "log out" the server cannot decide to leave them logged in.
func WithLogoutAudit(w audit.SplitLogger) LogoutOption {
	return func(o *logoutOpts) {
		o.audit = w
	}
}

// WithLogoutLogger installs a slog.Logger used to record non-fatal
// audit-write failures. Nil → slog.Default().
func WithLogoutLogger(l *slog.Logger) LogoutOption {
	return func(o *logoutOpts) {
		o.logger = l
	}
}

// Logout deletes the server-side session row (best-effort — a missing
// session is treated as already-logged-out), expires the client cookie,
// and redirects to /login. Mounted on the tenant-scoped chain but
// OUTSIDE the auth chain so users with a stale cookie can still hit it
// without a redirect loop.
//
// When a SplitLogger is supplied (WithLogoutAudit), a logout row is
// appended to audit_log_security AFTER the session row is deleted.
// actor_user_id is left as uuid.Nil — the audit ledger column is
// nullable (FK ON DELETE SET NULL on users(id), migration 0083) and
// the tenant Logout port does not surface the principal id. session_id
// in target preserves correlation with the (now-deleted) session row.
func Logout(svc LogoutDeleter, opts ...LogoutOption) http.HandlerFunc {
	if svc == nil {
		panic("handler: Logout deleter is nil")
	}
	o := logoutOpts{}
	for _, apply := range opts {
		apply(&o)
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, err := tenancy.FromContext(r.Context())
		if err != nil {
			http.Error(w, "tenant scope missing", http.StatusInternalServerError)
			return
		}
		var (
			sessionID       uuid.UUID
			hadValidSession bool
		)
		if value, err := sessioncookie.Read(r, sessioncookie.NameTenant); err == nil {
			if parsed, perr := uuid.Parse(value); perr == nil {
				sessionID = parsed
				hadValidSession = true
				_ = svc.Logout(r.Context(), tenant.ID, parsed)
			}
		}
		sessioncookie.ClearTenant(w)
		// Drop the CSRF cookie alongside the session when one was
		// presented — the next authenticated request must mint a
		// fresh token via Login, matching the per-session rotation
		// rule in ADR 0073 §D1/D3. Conditional emit keeps the
		// response bytes minimal when the client never carried a
		// __Host-csrf cookie (legacy session, programmatic client).
		if _, err := sessioncookie.Read(r, sessioncookie.NameCSRF); err == nil {
			sessioncookie.ClearCSRF(w)
		}
		if o.audit != nil && hadValidSession {
			tenantID := tenant.ID
			if werr := o.audit.WriteSecurity(r.Context(), audit.SecurityAuditEvent{
				Event:       audit.SecurityEventLogout,
				ActorUserID: uuid.Nil,
				TenantID:    &tenantID,
				Target: map[string]any{
					"session_id": sessionID.String(),
					"audience":   "tenant",
					"reason":     "user_initiated",
				},
			}); werr != nil {
				o.logger.WarnContext(r.Context(), "handler: logout audit write failed",
					slog.String("err", werr.Error()),
					slog.String("session_id_prefix", sessionID.String()[:8]),
				)
			}
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}
