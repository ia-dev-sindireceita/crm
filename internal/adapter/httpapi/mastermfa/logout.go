package mastermfa

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam/audit"
)

// LogoutHandlerConfig is the constructor input. Sessions is required;
// LoginPath defaults to /m/login.
//
// AuditLogger is optional (nil-safe). When non-nil the handler emits a
// SecurityEventLogout row to audit_log_security AFTER the session is
// deleted (SIN-63188 / Fase 6 PR6). The row is best-effort — a write
// failure is logged but does not prevent the cookie clear or the
// redirect. actor_user_id is the master operator id loaded via
// SessionStore.Get before Delete; tenant_id is left NULL (master rows
// have no tenant scope per the audit_log_security RLS rules).
type LogoutHandlerConfig struct {
	Sessions    SessionStore
	AuditLogger audit.SplitLogger
	Logger      *slog.Logger
	LoginPath   string
}

// LogoutHandler renders /m/logout. Unlike POST-only flows, the
// handler accepts both GET and POST so a stale-session operator who
// hits /m/logout from an idle browser can clear without a redirect
// loop (parallel to the tenant Logout shape — middleware/auth.go).
//
// On every request:
//  1. Reads __Host-sess-master via sessioncookie.Read.
//  2. If parseable, calls SessionStore.Get to capture the operator id
//     (best-effort — a missing row produces no audit), then calls
//     SessionStore.Delete (idempotent — a missing row is not an error
//     per the PR1 contract).
//  3. Calls sessioncookie.ClearMaster to drop the cookie.
//  4. If AuditLogger is non-nil and a session existed, appends a
//     SecurityEventLogout row to audit_log_security with the captured
//     operator id (SIN-63188).
//  5. 303 to LoginPath.
//
// A storage error during Delete is logged but NOT surfaced — the
// cookie clear and redirect proceed regardless. The end-state the
// operator cares about is "I am no longer signed in", and a stale
// row that the next sweep cleans up is harmless. ADR 0073 §D3 logout
// is best-effort by design.
type LogoutHandler struct {
	cfg LogoutHandlerConfig
}

// NewLogoutHandler validates inputs and returns the handler. nil
// Sessions panics at wire time per the project convention.
func NewLogoutHandler(cfg LogoutHandlerConfig) *LogoutHandler {
	if cfg.Sessions == nil {
		panic("mastermfa: NewLogoutHandler: Sessions is nil")
	}
	if cfg.LoginPath == "" {
		cfg.LoginPath = "/m/login"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &LogoutHandler{cfg: cfg}
}

// ServeHTTP implements http.Handler.
func (h *LogoutHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var (
		auditUserID    uuid.UUID
		auditSessionID uuid.UUID
		haveAudit      bool
	)

	if raw, err := sessioncookie.Read(r, sessioncookie.NameMaster); err == nil {
		if sessionID, perr := uuid.Parse(raw); perr == nil {
			auditSessionID = sessionID
			// Get first so we can surface the user id on the audit row;
			// best-effort: a missing or expired row simply means no audit
			// event is appended (we have no operator to attribute it to,
			// and the post-condition "logged out" is already true).
			if sess, gerr := h.cfg.Sessions.Get(r.Context(), sessionID); gerr == nil {
				auditUserID = sess.UserID
				haveAudit = true
			} else if !errors.Is(gerr, ErrSessionNotFound) && !errors.Is(gerr, ErrSessionExpired) {
				h.cfg.Logger.WarnContext(r.Context(), "mastermfa: logout: get failed",
					slog.String("err", gerr.Error()),
				)
			}
			if derr := h.cfg.Sessions.Delete(r.Context(), sessionID); derr != nil {
				if !errors.Is(derr, ErrSessionNotFound) {
					h.cfg.Logger.WarnContext(r.Context(), "mastermfa: logout: delete failed",
						slog.String("err", derr.Error()),
					)
				}
			}
		}
	}

	sessioncookie.ClearMaster(w)

	if h.cfg.AuditLogger != nil && haveAudit {
		if werr := h.cfg.AuditLogger.WriteSecurity(r.Context(), audit.SecurityAuditEvent{
			Event:       audit.SecurityEventLogout,
			ActorUserID: auditUserID,
			TenantID:    nil,
			Target: map[string]any{
				"session_id": auditSessionID.String(),
				"audience":   "master",
				"reason":     "user_initiated",
			},
		}); werr != nil {
			h.cfg.Logger.WarnContext(r.Context(), "mastermfa: logout audit write failed",
				slog.String("err", werr.Error()),
				slog.String("session_id_prefix", auditSessionID.String()[:8]),
			)
		}
	}

	// Cache headers ensure the redirect (which carries Set-Cookie
	// MaxAge=-1) is not stored by an intermediary.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	http.Redirect(w, r, h.cfg.LoginPath, http.StatusSeeOther)
}
