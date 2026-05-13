package mastermfa

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
)

// LogoutHandlerConfig is the constructor input. Sessions is required;
// LoginPath defaults to /m/login.
type LogoutHandlerConfig struct {
	Sessions  SessionStore
	Logger    *slog.Logger
	LoginPath string
}

// LogoutHandler renders /m/logout. Unlike POST-only flows, the
// handler accepts both GET and POST so a stale-session operator who
// hits /m/logout from an idle browser can clear without a redirect
// loop (parallel to the tenant Logout shape — middleware/auth.go).
//
// On every request:
//  1. Reads __Host-sess-master via sessioncookie.Read.
//  2. If parseable, calls SessionStore.Delete (idempotent — a missing
//     row is not an error per the PR1 contract).
//  3. Calls sessioncookie.ClearMaster to drop the cookie.
//  4. 303 to LoginPath.
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

	if raw, err := sessioncookie.Read(r, sessioncookie.NameMaster); err == nil {
		if sessionID, perr := uuid.Parse(raw); perr == nil {
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

	// Cache headers ensure the redirect (which carries Set-Cookie
	// MaxAge=-1) is not stored by an intermediary.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	http.Redirect(w, r, h.cfg.LoginPath, http.StatusSeeOther)
}
