package mastermfa

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// EnrollmentReader is the slice of mfa.SeedRepository the middleware
// needs to answer "is this master enrolled?". A narrow port keeps the
// middleware test surface tight — tests inject a one-method fake
// rather than the full SeedRepository.
type EnrollmentReader interface {
	LoadSeed(ctx context.Context, userID uuid.UUID) ([]byte, error)
}

// MFARequiredAuditor is the slice of mfa.AuditLogger that the
// middleware emits "denied" events through. Pulled out as a one-method
// port for the same reason as EnrollmentReader.
type MFARequiredAuditor interface {
	LogMFARequired(ctx context.Context, userID uuid.UUID, route, reason string) error
}

// RequireMasterMFAConfig is the constructor input. EnrollPath and
// VerifyPath default to ADR 0074 §3 / §4 ("/m/2fa/enroll",
// "/m/2fa/verify"); callers override only when wiring the handlers
// at non-default routes.
type RequireMasterMFAConfig struct {
	Enrollment EnrollmentReader
	Sessions   MasterSessionMFA
	Audit      MFARequiredAuditor
	Logger     *slog.Logger
	EnrollPath string
	VerifyPath string
}

// Reasons emitted with LogMFARequired so dashboards can split
// "user must enrol" from "user must re-verify in this login".
const (
	ReasonNotEnrolled = "not_enrolled"
	ReasonNotVerified = "not_verified"
)

// RequireMasterMFA returns a middleware that gates every wrapped
// route on:
//  1. The presence of a Master in the request context (upstream
//     master-auth middleware writes this; missing → 401).
//  2. The master being enrolled (master_mfa row exists).
//     mfa.ErrNotEnrolled → 303 to EnrollPath (ADR 0074 §3).
//  3. The current session being mfa-verified.
//     IsVerified == false → 303 to VerifyPath (§4).
//
// All redirect responses preserve the original URL in `?return=` so
// the verify/enrol handler can bounce the user back after a
// successful submission. The return value is server-side validated
// to be a relative path (no scheme, no host, starts with "/")
// before being placed in the URL — preventing open-redirect abuse.
//
// The middleware is deny-by-default — composing it on the master
// router ensures any new route under master.* inherits the gate
// without explicit per-handler wiring (ADR 0074 §3 secure-by-default).
func RequireMasterMFA(cfg RequireMasterMFAConfig) func(http.Handler) http.Handler {
	if cfg.Enrollment == nil {
		panic("mastermfa: RequireMasterMFA: Enrollment is nil")
	}
	if cfg.Sessions == nil {
		panic("mastermfa: RequireMasterMFA: Sessions is nil")
	}
	if cfg.Audit == nil {
		panic("mastermfa: RequireMasterMFA: Audit is nil")
	}
	enrollPath := cfg.EnrollPath
	if enrollPath == "" {
		enrollPath = "/m/2fa/enroll"
	}
	verifyPath := cfg.VerifyPath
	if verifyPath == "" {
		verifyPath = "/m/2fa/verify"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			master, ok := MasterFromContext(r.Context())
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			// Step 2: enrolment check.
			_, err := cfg.Enrollment.LoadSeed(r.Context(), master.ID)
			if errors.Is(err, mfa.ErrNotEnrolled) {
				_ = cfg.Audit.LogMFARequired(r.Context(), master.ID, r.URL.Path, ReasonNotEnrolled)
				redirectWithReturn(w, r, enrollPath)
				return
			}
			if err != nil {
				logger.ErrorContext(r.Context(), "mastermfa: enrolment read failed",
					slog.String("user_id", master.ID.String()),
					slog.String("error", err.Error()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}

			// Step 3: session-mfa-verified check.
			verified, err := cfg.Sessions.IsVerified(r)
			if err != nil {
				logger.ErrorContext(r.Context(), "mastermfa: session mfa state read failed",
					slog.String("user_id", master.ID.String()),
					slog.String("error", err.Error()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			if !verified {
				_ = cfg.Audit.LogMFARequired(r.Context(), master.ID, r.URL.Path, ReasonNotVerified)
				redirectWithReturn(w, r, verifyPath)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// redirectWithReturn issues a 303 See Other to target with the
// original path placed in `?return=`. ADR 0074 §3 specifies 303 (not
// 302) so a POST that lands on a gated route is explicitly converted
// to a GET on the redirect.
//
// The original URL is validated to be a relative path; absolute URLs
// or scheme-relative shapes are dropped so an attacker cannot use a
// crafted Referer/Host to bounce the user off-site after they enrol.
func redirectWithReturn(w http.ResponseWriter, r *http.Request, target string) {
	original := r.URL.RequestURI()
	if !isSafeReturnPath(original) {
		// Fallback: send to target without `?return=`.
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	q := url.Values{}
	q.Set("return", original)
	http.Redirect(w, r, target+"?"+q.Encode(), http.StatusSeeOther)
}

// isSafeReturnPath confirms s is a server-relative URL: starts with
// "/", does not start with "//", does not contain a scheme. This is
// the same gate ResolveReturn (verify handler) applies on the way
// back, so the round-trip never accepts an absolute URL.
func isSafeReturnPath(s string) bool {
	if !strings.HasPrefix(s, "/") {
		return false
	}
	if strings.HasPrefix(s, "//") {
		return false
	}
	// Any colon before the first '/' would make it scheme-relative
	// (we already handled the leading '/'); a colon anywhere else is
	// fine in a path/query.
	return true
}

// ResolveReturn is the verify handler's safe extractor of the
// `?return=` query value. Returns the cleaned path or fallback if
// the supplied value is unsafe (absolute URL, off-site, or empty).
//
// Exposed so the verify handler can call it after a successful
// submission without re-implementing the path-safety gate.
func ResolveReturn(raw, fallback string) string {
	if raw == "" {
		return fallback
	}
	decoded, err := url.QueryUnescape(raw)
	if err != nil {
		return fallback
	}
	if !isSafeReturnPath(decoded) {
		return fallback
	}
	return decoded
}
