package handler

import (
	"html/template"
	"net/http"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/views"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// HelloTenantDeps reports which post-login surfaces should render as
// live links on /hello-tenant. Each flag mirrors a `if deps.WebX != nil`
// guard in router.go — true means the underlying handler is mounted in
// the current process and the link goes live; false means the link
// renders disabled with an "(indisponível neste ambiente)" hint so the
// gap is visible to the operator (SIN-63773 AC §2).
//
// Only presence/absence travels into the handler, not the handlers
// themselves: the hexagonal boundary keeps router internals out of the
// template-rendering layer.
type HelloTenantDeps struct {
	FunnelEnabled      bool
	FunnelRulesEnabled bool
	CatalogEnabled     bool
	CampaignsEnabled   bool
	PrivacyEnabled     bool
	AIPolicyEnabled    bool
	ConsentEnabled     bool
}

// NewHelloTenant returns the post-login landing handler with a typed
// surfaces index derived from deps. The index lets a freshly-logged-in
// operator reach every Fase 3–6 surface without typing a URL, fixing
// the "logged in but no nav" gap reported in SIN-63773.
//
// The handler stays behind the same RequireAuth + RequireAction wrap in
// router.go — this constructor only changes the body rendered when the
// gate allows the request through.
func NewHelloTenant(deps HelloTenantDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, err := tenancy.FromContext(r.Context())
		if err != nil {
			http.Error(w, "tenant scope missing", http.StatusInternalServerError)
			return
		}
		sess, ok := middleware.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "session missing", http.StatusInternalServerError)
			return
		}
		data := struct {
			TenantName       string
			UserID           string
			CSRFToken        string
			TenantThemeStyle template.CSS
			CSPNonce         string
			Surfaces         []views.Surface
		}{
			TenantName:       tenant.Name,
			UserID:           sess.UserID.String(),
			CSRFToken:        sess.CSRFToken,
			TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
			CSPNonce:         csp.Nonce(r.Context()),
			Surfaces:         buildHelloSurfaces(deps),
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := views.Hello.ExecuteTemplate(w, "layout", data); err != nil {
			http.Error(w, "render error", http.StatusInternalServerError)
			return
		}
	}
}

// HelloTenant is the zero-deps shim retained for backward compatibility
// with router_test.go fixtures and handler-level tests that wire the
// route without the per-feature WebX handlers. Every surface renders
// disabled in this mode — equivalent to a deployment where no Fase 3–6
// adapter is mounted.
//
// Production wiring goes through NewHelloTenant; see router.go.
func HelloTenant(w http.ResponseWriter, r *http.Request) {
	NewHelloTenant(HelloTenantDeps{})(w, r)
}

// buildHelloSurfaces returns the post-login index entries in the order
// defined by SIN-63774. Order is intentionally stable so QA scripts and
// future screenshot diffs are deterministic.
func buildHelloSurfaces(deps HelloTenantDeps) []views.Surface {
	return []views.Surface{
		{Label: "Funil de conversas", Path: "/funnel", Available: deps.FunnelEnabled},
		{Label: "Regras do funil", Path: "/funnel/rules", Available: deps.FunnelRulesEnabled},
		{Label: "Catálogo de produtos", Path: "/catalog", Available: deps.CatalogEnabled},
		{Label: "Campanhas", Path: "/campaigns", Available: deps.CampaignsEnabled},
		{Label: "Privacidade e DPA", Path: "/settings/privacy", Available: deps.PrivacyEnabled},
		{Label: "Política de IA", Path: "/settings/ai-policy", Available: deps.AIPolicyEnabled},
		{Label: "Banner de consentimento", Path: "/consent/cookies-banner", Available: deps.ConsentEnabled},
	}
}
