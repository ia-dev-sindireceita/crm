package mastermfa

import (
	"embed"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
)

// EnrollStartHandler renders GET /m/2fa/enroll — the server-rendered
// bootstrap surface required by ADR 0074 §1 ("redirecionado para GET
// /m/2fa/enroll, HTMX-first, server-rendered").
//
// The route was split from EnrollHandler (POST mint) per the CTO
// decision on SIN-65264: keeping the mint POST-only preserves the
// "mint a fresh seed on every call" footgun guard (idempotent GET would
// invalidate the previous recovery-code set on reload), while this GET
// gives a not-enrolled master a reachable entry point. RequireMasterMFA
// self-excludes the enroll path so a not-enrolled master is no longer
// bounced in an infinite 303 loop, and RequireMasterAuth still gates the
// route, so only an authenticated master session lands here.
//
// The page carries the original ?return= through to the POST so the
// operator is delivered to the URL they were trying to reach once the
// enroll → verify round-trip completes.
type EnrollStartHandler struct {
	logger *slog.Logger
	tmpl   *template.Template
}

// NewEnrollStartHandler parses the embedded template eagerly; a parse
// failure means the binary is malformed (go:embed) so it panics, matching
// the EnrollHandler / VerifyHandler constructor convention.
func NewEnrollStartHandler(logger *slog.Logger) *EnrollStartHandler {
	if logger == nil {
		logger = slog.Default()
	}
	tmpl, err := template.ParseFS(enrollStartTemplates, "templates/enroll_start.html")
	if err != nil {
		panic("mastermfa: parse enroll_start template: " + err.Error())
	}
	return &EnrollStartHandler{logger: logger, tmpl: tmpl}
}

// ServeHTTP implements http.Handler. GET renders the start page; any
// other verb is 405 (the mint lives on POST at the same path, handled by
// EnrollHandler — this handler is wired to GET only, so a non-GET here is
// a router wiring error, surfaced loudly).
func (h *EnrollStartHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	master, ok := MasterFromContext(r.Context())
	if !ok {
		// Deny-by-default: RequireMasterAuth should have populated the
		// context. A miss is a wiring bug — fail closed with 401.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	data := enrollStartViewData{
		ReturnPath:       ResolveReturn(r.URL.Query().Get("return"), ""),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
	}
	if err := h.tmpl.ExecuteTemplate(w, "enroll_start.html", data); err != nil {
		h.logger.ErrorContext(r.Context(), "mastermfa: enroll start render failed",
			slog.String("user_id", master.ID.String()),
			slog.String("error", err.Error()),
		)
	}
}

// enrollStartViewData is the strict (strings-only) shape passed to the
// template.
type enrollStartViewData struct {
	// ReturnPath is the validated server-relative destination the operator
	// was trying to reach; threaded through the POST form as a hidden field
	// so the post-verify redirect lands there. Empty when absent/unsafe.
	ReturnPath string
	// TenantThemeStyle carries the per-request runtime theming inline style
	// (SIN-63085); zero-value by default on the master console.
	TenantThemeStyle template.CSS
	// CSPNonce is the per-request CSP nonce (SIN-63275); empty fails closed.
	CSPNonce string
}

//go:embed templates/enroll_start.html
var enrollStartTemplates embed.FS
