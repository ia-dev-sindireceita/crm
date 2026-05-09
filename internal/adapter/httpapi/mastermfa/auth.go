package mastermfa

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
)

// MasterUserDirectory is the userID → email lookup the master-auth
// middleware needs to populate Master.Email after a session-id round
// trip. The master_session row holds only user_id; email lives on the
// users table. A narrow port keeps the middleware test surface tight
// — tests inject a one-method fake instead of a full users adapter.
//
// EmailFor MUST return ("", nil) when the row exists but has no email
// (defensive — production data has NOT NULL on email, but the contract
// avoids forcing every fake to choose a sentinel). Any non-nil error
// is treated as a transient storage failure and surfaces a 500 from
// the middleware.
type MasterUserDirectory interface {
	EmailFor(ctx context.Context, userID uuid.UUID) (string, error)
}

// MasterSessionAuditor is the slice of the master-side audit logger
// the auth middleware needs for the master.session.hard_cap_hit event
// (SIN-62418 / ADR 0073 §D3). Defining the port narrowly here keeps
// the middleware test surface tight — production wires the slog
// adapter (internal/adapter/audit/slog.MFAAudit) and tests inject a
// recording fake.
//
// The auditor is invoked exactly once per hard-cap hit, immediately
// before the cookie clear + redirect. Implementations MUST NOT block
// the request (the redirect proceeds even if the audit write fails).
type MasterSessionAuditor interface {
	LogHardCapHit(ctx context.Context, userID, sessionID uuid.UUID, createdAt, now time.Time, route string) error
}

// RequireMasterAuthConfig is the constructor input. LoginPath defaults
// to "/m/login"; callers override only when wiring at non-default
// routes. IdleTTL is the master idle timeout the middleware bumps the
// session row by on every authenticated request — ADR 0073 §D3 sets
// the master idle to 15 minutes.
//
// Auditor is the optional audit sink for the master.session.hard_cap_hit
// event (SIN-62418). nil keeps the middleware behaving exactly as it
// did pre-SIN-62418 — used by tests that don't wire an audit recorder;
// production wires the slog MFAAudit adapter so dashboards can split
// "hard cap hit" from "idle timeout".
//
// Now is an overridable clock (defaults to time.Now().UTC()) so tests
// can pin the audit timestamp deterministically without dragging a
// frozen clock through every dep.
type RequireMasterAuthConfig struct {
	Sessions  SessionStore
	Directory MasterUserDirectory
	Auditor   MasterSessionAuditor
	Logger    *slog.Logger
	LoginPath string
	IdleTTL   time.Duration
	Now       func() time.Time
}

// DefaultMasterIdleTTL is the ADR 0073 §D3 master idle timeout. The
// auth middleware uses this to bump expires_at on every request so an
// active operator does not get logged out mid-session, while an
// idle-for-15-minutes operator's next request hits an expired row and
// is bounced to /m/login.
const DefaultMasterIdleTTL = 15 * time.Minute

// RequireMasterAuth returns a middleware that gates every wrapped
// route on a valid master session cookie:
//
//  1. Reads __Host-sess-master via sessioncookie.Read.
//  2. Parses the value as a uuid.
//  3. Calls SessionStore.Get to validate.
//  4. On miss / expired / parse error → 303 to LoginPath?next=<original>.
//  5. On success → calls SessionStore.Touch to bump idle TTL, looks up
//     email via Directory, and writes Master{ID, Email} into ctx via
//     WithMaster.
//
// All redirect-to-login outcomes preserve the original URL in
// `?next=...` (validated as a relative path so a hostile Referer/Host
// cannot bounce the operator off-site after sign-in). A 401 path is
// intentionally NOT used: the master console is HTML-only, browser-
// driven; 303 keeps the UX continuous and matches the tenant Auth
// shape (internal/adapter/httpapi/middleware/auth.go).
//
// Storage failures (non-sentinel SessionStore errors) collapse to 500.
// ADR 0074 §3 deny-by-default: a transient pgx outage MUST NOT
// silently grant access by skipping the gate.
//
// Constructor panics on missing required deps so a misconfigured router
// fails loudly at wire time (consistent with EnrollHandler /
// VerifyHandler shape).
func RequireMasterAuth(cfg RequireMasterAuthConfig) func(http.Handler) http.Handler {
	if cfg.Sessions == nil {
		panic("mastermfa: RequireMasterAuth: Sessions is nil")
	}
	if cfg.Directory == nil {
		panic("mastermfa: RequireMasterAuth: Directory is nil")
	}
	loginPath := cfg.LoginPath
	if loginPath == "" {
		loginPath = "/m/login"
	}
	idleTTL := cfg.IdleTTL
	if idleTTL <= 0 {
		idleTTL = DefaultMasterIdleTTL
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := sessioncookie.Read(r, sessioncookie.NameMaster)
			if err != nil {
				redirectToMasterLogin(w, r, loginPath)
				return
			}
			sessionID, err := uuid.Parse(raw)
			if err != nil {
				redirectToMasterLogin(w, r, loginPath)
				return
			}
			sess, err := cfg.Sessions.Get(r.Context(), sessionID)
			if err != nil {
				if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrSessionExpired) {
					redirectToMasterLogin(w, r, loginPath)
					return
				}
				logger.ErrorContext(r.Context(), "mastermfa: session get failed",
					slog.String("error", err.Error()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}

			// Idle-bump: extend expires_at by idleTTL. A Touch failure on
			// ErrSessionNotFound is benign (race with a logout in another
			// tab) and falls back to the redirect; any other error 500s
			// rather than silently letting the request proceed without the
			// idle bump.
			//
			// ErrSessionHardCap (SIN-62418) is the storage layer reporting
			// that this request landed at or after created_at + 4h. The
			// row has already been deleted inside the Touch transaction;
			// here we mirror the idle-timeout UX (clear cookie + redirect
			// to /m/login) and emit master.session.hard_cap_hit so
			// dashboards can split breach attempts from benign churn.
			if err := cfg.Sessions.Touch(r.Context(), sessionID, idleTTL); err != nil {
				if errors.Is(err, ErrSessionHardCap) {
					if cfg.Auditor != nil {
						_ = cfg.Auditor.LogHardCapHit(r.Context(), sess.UserID, sessionID, sess.CreatedAt, now(), r.URL.Path)
					}
					sessioncookie.ClearMaster(w)
					redirectToMasterLogin(w, r, loginPath)
					return
				}
				if errors.Is(err, ErrSessionNotFound) {
					redirectToMasterLogin(w, r, loginPath)
					return
				}
				logger.ErrorContext(r.Context(), "mastermfa: session touch failed",
					slog.String("user_id", sess.UserID.String()),
					slog.String("error", err.Error()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}

			email, err := cfg.Directory.EmailFor(r.Context(), sess.UserID)
			if err != nil {
				logger.ErrorContext(r.Context(), "mastermfa: directory email lookup failed",
					slog.String("user_id", sess.UserID.String()),
					slog.String("error", err.Error()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}

			ctx := WithMaster(r.Context(), Master{ID: sess.UserID, Email: email})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// redirectToMasterLogin issues a 303 See Other to loginPath with the
// original request URI in `?next=`. Mirrors the tenant
// middleware.redirectToLogin shape: relative-path-only, host/scheme
// inferred by the browser at redirect time.
//
// Unsafe `next` values (absolute URLs, scheme-relative shapes) are
// dropped — the redirect still issues, just without a `?next=`. The
// login handler re-validates `next` on the way back out, so the round-
// trip never accepts an absolute URL.
func redirectToMasterLogin(w http.ResponseWriter, r *http.Request, loginPath string) {
	next := r.URL.RequestURI()
	if !isSafeReturnPath(next) {
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
		return
	}
	q := url.Values{}
	q.Set("next", next)
	http.Redirect(w, r, loginPath+"?"+q.Encode(), http.StatusSeeOther)
}
