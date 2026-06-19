package mastermfa

import (
	"context"
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// Enroller is the slice of mfa.Service the enrol handler depends on.
// Defining a narrow port here lets tests inject a fake without
// dragging the full Service into the mocks.
type Enroller interface {
	Enroll(ctx context.Context, userID uuid.UUID, label string) (mfa.EnrollResult, error)
}

// EnrollHandler renders POST /m/2fa/enroll. The route is POST-only
// because it mints a fresh seed + recovery codes on every call —
// idempotent GETs would invalidate the previous set on reload, which
// is a footgun. CSRF protection is supplied by the upstream
// httpapi/csrf RequireCSRF middleware (chained at router wire-time);
// this handler does not re-check the CSRF token itself.
type EnrollHandler struct {
	enroller Enroller
	logger   *slog.Logger
	tmpl     *template.Template
}

// NewEnrollHandler validates inputs and returns the handler. The
// embedded template is parsed eagerly: a parse failure means the
// binary itself is malformed (the template is //go:embed'ed from the
// source tree), so the constructor panics rather than returning an
// error — there is no recovery path callers could plausibly take. nil
// enroller is similarly a programmer error and panics at wire time.
func NewEnrollHandler(enroller Enroller, logger *slog.Logger) *EnrollHandler {
	if enroller == nil {
		panic("mastermfa: NewEnrollHandler: enroller is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	tmpl, err := template.ParseFS(enrollTemplates, "templates/enroll_result.html")
	if err != nil {
		panic("mastermfa: parse embedded template: " + err.Error())
	}
	return &EnrollHandler{enroller: enroller, logger: logger, tmpl: tmpl}
}

// ServeHTTP implements http.Handler.
func (h *EnrollHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	master, ok := MasterFromContext(r.Context())
	if !ok {
		// Deny-by-default per ADR 0074 §3. The same shape as
		// auth/Auth's redirectToLogin would be ideal in production,
		// but that helper is master-session-aware and master sessions
		// don't exist yet in this repo. Until then, surface 401 so
		// any wire-up bug is loud.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	res, err := h.enroller.Enroll(r.Context(), master.ID, master.Email)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "mastermfa: enroll failed",
			slog.String("user_id", master.ID.String()),
			slog.String("error", err.Error()),
		)
		// Generic message — never leak the internal failure.
		http.Error(w, "could not start enrolment, please retry", http.StatusInternalServerError)
		return
	}

	// Cache headers: the response embeds plaintext recovery codes
	// that MUST NOT live in any browser cache or proxy. Cache-Control
	// + Pragma combine to defeat HTTP/1.0 and HTTP/1.1 intermediaries.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Thread the bootstrap ?return= (carried as a hidden field from the
	// enroll start page) into the verify link so the post-verify redirect
	// lands on the URL the operator originally requested. r.FormValue
	// parses the POST body; ResolveReturn drops any unsafe/absolute value.
	verifyHref := "/m/2fa/verify"
	if ret := ResolveReturn(r.FormValue("return"), ""); ret != "" {
		q := url.Values{}
		q.Set("return", ret)
		verifyHref = verifyHref + "?" + q.Encode()
	}

	data := enrollViewData{
		OTPAuthURI:       res.OTPAuthURI,
		SecretEncoded:    res.SecretEncoded,
		RecoveryCodes:    formatRecoveryCodes(res.RecoveryCodes),
		VerifyHref:       verifyHref,
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
	}
	if err := h.tmpl.ExecuteTemplate(w, "enroll_result.html", data); err != nil {
		// At this point we've already written headers — log and let
		// the partial response surface to the caller. The view data
		// is only strings so a runtime template error here is
		// effectively impossible; we still log defensively.
		h.logger.ErrorContext(r.Context(), "mastermfa: render failed",
			slog.String("error", err.Error()),
		)
		return
	}
}

// enrollViewData is the strict shape passed to the template. Only
// strings — no derived types or methods — so the template does no
// rendering beyond textual interpolation.
type enrollViewData struct {
	OTPAuthURI    string
	SecretEncoded string
	RecoveryCodes []string
	// VerifyHref is the link to /m/2fa/verify (with the bootstrap
	// ?return= preserved when present) that completes the enroll → verify
	// round-trip. SIN-65264.
	VerifyHref string
	// TenantThemeStyle carries the per-request runtime theming inline
	// style (SIN-63085).
	TenantThemeStyle template.CSS
	// CSPNonce is the per-request CSP nonce (SIN-63275). Stamped on
	// every <style> tag in enroll_result.html. Empty string means the
	// request was not routed through csp.Middleware — the resulting
	// nonce="" never matches a CSP directive, fail-closed.
	CSPNonce string
}

// formatRecoveryCodes renders each plaintext code with the midpoint
// dash from mfa.FormatRecoveryCode (e.g. "ABCDE-FGHIJ"). The hyphen
// is presentation-only; the verifier strips it via NormalizeRecoveryCode.
func formatRecoveryCodes(codes []string) []string {
	out := make([]string, len(codes))
	for i, c := range codes {
		out[i] = mfa.FormatRecoveryCode(c)
	}
	return out
}

//go:embed templates/enroll_result.html
var enrollTemplates embed.FS
