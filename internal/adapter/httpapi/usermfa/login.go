package usermfa

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/loginhandler"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/views"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// DefaultPendingTTL is the lifetime of a pre-MFA pending row. The
// user has this long between password-auth and TOTP-verify before
// the pending row expires and they need to re-authenticate.
const DefaultPendingTTL = 5 * time.Minute

// PendingCreator is the slice of TenantUserMFAPending the login
// wrapper needs. Keeping it narrow lets tests inject a fake without
// dragging in *pgxpool.Pool.
type PendingCreator interface {
	Create(ctx context.Context, userID uuid.UUID, ttl time.Duration, nextPath string) (Pending, error)
}

// RequirementReader returns the MFA-requirement snapshot for the
// authenticated principal. The postgres adapter is
// TenantUserMFARequirement (joins users + user_mfa); tests inject a
// fake.
type RequirementReader interface {
	Load(ctx context.Context, userID uuid.UUID) (Requirement, error)
}

// Requirement mirrors postgres.UserMFARequirement at the HTTP layer
// boundary so this package does not import the postgres adapter type
// directly. A tiny boundary type beats coupling.
type Requirement struct {
	TOTPRequired bool
	TOTPEnrolled bool
}

// LoginAuthenticator is the slice of iam.Service.Login needed by
// LoginPostMFA — identical to handler.LoginAuthenticator so cmd/server
// can re-use the same wiring.
type LoginAuthenticator interface {
	Login(ctx context.Context, host, email, password string, ipAddr net.IP, userAgent, route string) (iam.Session, error)
}

// SessionDeleter is the slice of iam.SessionStore the login wrapper
// uses to roll back the just-created session row when MFA is
// required. The cookie was not written so this delete cleans up the
// orphan row.
type SessionDeleter interface {
	Delete(ctx context.Context, tenantID, sessionID uuid.UUID) error
}

// LoginConfig wires the dependencies the MFA-aware login handler
// needs.
type LoginConfig struct {
	IAM          LoginAuthenticator
	Sessions     SessionDeleter
	Pendings     PendingCreator
	Requirements RequirementReader
	PendingTTL   time.Duration
	VerifyPath   string
	SetupPath    string
	FallbackOK   string
	Logger       *slog.Logger
	// Branding is the optional tenant-settings read port (SIN-63963 /
	// UX-F4) used by the credential-failure re-render to brand the card
	// after a bad password. Nil keeps the word-mark + footer fallback.
	Branding tenancy.BrandingReader
}

// LoginPost returns the POST /login handler. Behaviour:
//
//   - Calls IAM.Login to validate (host, email, password). On failure
//     it falls through to the same error rendering the password-only
//     LoginPost uses.
//   - Calls Requirements.Load(userID). If TOTP is required, the
//     just-created session row is deleted, a pending row is created,
//     the __Host-mfa-pending cookie is set, and the response 303s to
//     /admin/2fa/setup (when not enrolled) or /admin/2fa/verify (when
//     enrolled).
//   - Otherwise sets __Host-sess-tenant + __Host-csrf and redirects
//     to the validated `?next=`.
func LoginPost(cfg LoginConfig) http.HandlerFunc {
	if cfg.IAM == nil {
		panic("usermfa: LoginPost IAM is nil")
	}
	if cfg.Sessions == nil {
		panic("usermfa: LoginPost Sessions is nil")
	}
	if cfg.Pendings == nil {
		panic("usermfa: LoginPost Pendings is nil")
	}
	if cfg.Requirements == nil {
		panic("usermfa: LoginPost Requirements is nil")
	}
	if cfg.PendingTTL <= 0 {
		cfg.PendingTTL = DefaultPendingTTL
	}
	if cfg.VerifyPath == "" {
		cfg.VerifyPath = "/admin/2fa/verify"
	}
	if cfg.SetupPath == "" {
		cfg.SetupPath = "/admin/2fa/setup"
	}
	if cfg.FallbackOK == "" {
		cfg.FallbackOK = "/hello-tenant"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		email := strings.TrimSpace(r.PostFormValue("email"))
		password := r.PostFormValue("password")
		next := sanitizeNext(r.PostFormValue("next"), cfg.FallbackOK)

		ipAddr := parseRemoteIP(r.RemoteAddr)
		sess, err := cfg.IAM.Login(r.Context(), r.Host, email, password, ipAddr, r.UserAgent(), r.URL.Path)
		if err != nil {
			if errors.Is(err, iam.ErrInvalidCredentials) {
				renderLoginError(w, r, next, cfg.Branding)
				return
			}
			loginhandler.WriteLoginError(w, r, err, cfg.Logger)
			return
		}
		req, err := cfg.Requirements.Load(r.Context(), sess.UserID)
		if err != nil {
			cfg.Logger.ErrorContext(r.Context(), "usermfa: load mfa requirement failed",
				slog.String("user_id", sess.UserID.String()),
				slog.String("err", err.Error()),
			)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if !req.TOTPRequired {
			sessioncookie.SetTenant(w, sess.ID.String(), 0)
			if sess.CSRFToken != "" {
				sessioncookie.SetCSRF(w, sess.CSRFToken, 0)
			}
			http.Redirect(w, r, next, http.StatusFound)
			return
		}
		// AC #1: roll back the just-created session row so no cookie
		// is leaked to an MFA-required principal. The pending row is
		// the access predicate until /admin/2fa/verify completes.
		if err := cfg.Sessions.Delete(r.Context(), sess.TenantID, sess.ID); err != nil {
			cfg.Logger.WarnContext(r.Context(), "usermfa: rollback session on mfa-pending failed",
				slog.String("user_id", sess.UserID.String()),
				slog.String("err", err.Error()),
			)
		}
		pending, err := cfg.Pendings.Create(r.Context(), sess.UserID, cfg.PendingTTL, next)
		if err != nil {
			cfg.Logger.ErrorContext(r.Context(), "usermfa: create pending failed",
				slog.String("user_id", sess.UserID.String()),
				slog.String("err", err.Error()),
			)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		sessioncookie.SetTenantPending(w, pending.ID.String(), int(cfg.PendingTTL.Seconds()))
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
		w.Header().Set("Pragma", "no-cache")
		target := cfg.VerifyPath
		if !req.TOTPEnrolled {
			target = cfg.SetupPath
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	}
}

func renderLoginError(w http.ResponseWriter, r *http.Request, next string, reader tenancy.BrandingReader) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	// SIN-63941 / UX-F4 — the MFA-aware credential-failure path renders
	// the same views.Login template as the legacy handler, so the data
	// struct carries TenantName/TenantLogo/WhiteLabel/TenantThemeStyle to
	// keep the card branded after a bad-password attempt. tenancy is
	// already on the request via middleware.TenantScope. SIN-63963 wires
	// the tenant-settings read port here so logo + white-label match the
	// GET /login render; a nil reader or any read failure degrades to the
	// word-mark + platform footer rather than failing the page.
	data := struct {
		Next             string
		Error            string
		CSRFToken        string
		CSPNonce         string
		TenantName       string
		TenantLogo       string
		WhiteLabel       bool
		TenantThemeStyle template.CSS
	}{
		Next:             next,
		Error:            "Email ou senha inválidos.",
		CSPNonce:         csp.Nonce(r.Context()),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
	}
	if t, err := tenancy.FromContext(r.Context()); err == nil && t != nil {
		data.TenantName = t.Name
		if reader != nil {
			if b, lerr := reader.LoadBranding(r.Context(), t.ID); lerr == nil {
				data.TenantLogo = b.LogoURL
				data.WhiteLabel = b.WhiteLabel
			} else if !errors.Is(lerr, tenancy.ErrTenantNotFound) {
				logger := slog.Default()
				logger.WarnContext(r.Context(), "usermfa: load login branding failed",
					slog.String("tenant_id", t.ID.String()),
					slog.String("err", lerr.Error()),
				)
			}
		}
	}
	_ = views.Login.ExecuteTemplate(w, "layout", data)
}

func sanitizeNext(raw, fallback string) string {
	return safeNext(raw, fallback)
}

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
