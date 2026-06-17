package master

// SIN-63956 / master-impersonation-spec rev 2 §2 — server-side helper
// that translates the middleware's active impersonation envelope into
// the view-model the master/shell templates render the red banner
// against. The handler calls BuildImpersonationContext per request
// before executing its template; nil return means "no envelope" and
// the template's {{with}} guard suppresses the banner.
//
// The view-model is rooted in internal/web/shell.ImpersonationContext
// so the F1 shell layout can pick it up from page data without a new
// type dependency on internal/web/master. Master pages that have not
// yet migrated to shell.Layout still embed *shell.ImpersonationContext
// on their pageData and render the same banner subtemplate (see
// templates.go banner subtree).

import (
	"context"
	"html/template"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/shell"
)

// banner template fragment shared by every master full-page layout
// (tenants list, tenant detail, grants, grant-requests inbox, etc.).
// The fragment is parsed once and AddParseTree'd into each layout in
// the per-file init() — see templates.go / grants_templates.go /
// grant_requests_templates.go.
const impersonationBannerSource = `{{define "shell_impersonation_banner"}}
{{- with .ActiveImpersonation}}
<aside class="shell__impersonation-banner"
       role="alert"
       aria-label="Modo impersonação ativo"
       data-impersonation-banner="true"
       data-expires-at="{{formatImpersonationISO .ExpiresAt}}"
       data-server-now="{{formatImpersonationISO .ServerNow}}">
  <form method="POST"
        action="/master/impersonation/end"
        id="impersonation-end-form"
        class="shell__impersonation-end">
    {{- with .EndCSRFToken}}{{csrfHiddenForToken .}}{{end}}
  </form>
  <button type="submit"
          form="impersonation-end-form"
          class="shell__impersonation-end-btn"
          aria-label="Sair da impersonação"
          tabindex="0"
          autofocus>SAIR DA IMPERSONAÇÃO</button>
  <span class="shell__impersonation-pill">
    <span aria-hidden="true" class="shell__impersonation-pill-icon">{{icon "octagon-alert"}}</span>
    IMPERSONANDO
  </span>
  <span class="shell__impersonation-tenant">
    Tenant: <strong>{{.TenantName}}</strong>
    {{- with .TenantSlug}} (<code>{{.}}</code>){{end}}
  </span>
  <span class="shell__impersonation-countdown"
        aria-live="polite"
        aria-atomic="true"
        data-impersonation-countdown="true"
        tabindex="0">
    <noscript>ativa até {{formatImpersonationISO .ExpiresAt}}</noscript>
  </span>
  <span class="shell__impersonation-reason"
        title="{{.Reason}}">
    Motivo: {{truncateImpersonationReason .Reason}}
  </span>
</aside>
{{- end}}
{{end}}

{{define "shell_audit_feed_chip"}}
{{- with .ActiveImpersonation}}
<div class="master-audit-feed" data-master-audit-feed>
  <button type="button"
          class="master-audit-feed__chip"
          aria-expanded="false"
          aria-controls="master-audit-feed-panel">
    <span aria-hidden="true">{{icon "zap"}}</span> Auditoria
  </button>
  <section class="master-audit-feed__panel"
           id="master-audit-feed-panel"
           role="region"
           aria-label="Feed de auditoria da impersonação">
    <p class="master-audit-feed__hint">
      Eventos desta impersonação são registrados em
      <code>audit_log_security</code> e listados via
      <a href="/master/impersonation/feed">SSE feed</a>.
    </p>
    <noscript>
      <p>JavaScript desabilitado — abra o feed manualmente.</p>
    </noscript>
  </section>
</div>
{{- end}}
{{end}}`

// impersonationBannerTmpl is the parse tree shared by every master
// layout (see init() in templates.go etc.). Funcs match what every
// page already declares — formatImpersonationISO and
// truncateImpersonationReason are added to the master FuncMap below.
var impersonationBannerTmpl = template.Must(template.New("shell_impersonation_banner.bundle").
	Funcs(template.FuncMap{
		"formatImpersonationISO":      formatImpersonationISO,
		"truncateImpersonationReason": truncateImpersonationReason,
		"csrfHiddenForToken":          csrfHiddenForToken,
		"icon":                        iconSVG,
	}).
	Parse(impersonationBannerSource))

func formatImpersonationISO(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func truncateImpersonationReason(reason string) string {
	const limit = 80
	if len(reason) <= limit {
		return reason
	}
	return reason[:limit] + "…"
}

// csrfHiddenForToken renders the same hidden input that csrf.FormHidden
// emits, but threaded via the impersonation context's EndCSRFToken so
// the banner template can render without a top-level CSRFInput field.
// Duplicating the input markup keeps the banner subtemplate FuncMap
// self-contained — adding a runtime dependency on csrf in the banner
// would force every page to import it, which they already do via the
// page-level CSRFInput field.
func csrfHiddenForToken(token string) template.HTML {
	return template.HTML(`<input type="hidden" name="csrf_token" value="` +
		template.HTMLEscapeString(token) + `">`)
}

// BuildImpersonationContext returns the banner view-model for the
// active envelope, or nil when no envelope is bound to the request.
// The handler MUST call this before executing the page template so the
// banner renders consistently across every authed master route (spec
// §2.3 + §10.2 #9).
//
// Failure to resolve the tenant degrades to a banner with TenantName ==
// short-form fallback rather than dropping the banner entirely — the
// security signal "you are impersonating" MUST remain visible even on
// a partial fetch failure.
func BuildImpersonationContext(r *http.Request, tenants tenancy.ByIDResolver, endCSRFToken string, now func() time.Time) *shell.ImpersonationContext {
	active, ok := middleware.ActiveImpersonation(r.Context())
	if !ok || active == nil {
		return nil
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	name, slug := resolveBannerTenant(r.Context(), tenants, active.TargetTenantID)
	return &shell.ImpersonationContext{
		TenantName:   name,
		TenantSlug:   slug,
		Reason:       active.Reason,
		ExpiresAt:    active.ExpiresAt,
		ServerNow:    now(),
		EndCSRFToken: endCSRFToken,
	}
}

// resolveBannerTenant returns (display_name, slug). On error the
// fallback is the bare tenant UUID so the banner remains informative
// without leaking the resolver error to the user. The middleware has
// already gated the request on a valid envelope so a missing tenant
// here is a stale catalog read, not an attacker-induced state.
func resolveBannerTenant(ctx context.Context, tenants tenancy.ByIDResolver, id uuid.UUID) (string, string) {
	if tenants == nil {
		return shortHex(id[:]), ""
	}
	t, err := tenants.ResolveByID(ctx, id)
	if err != nil || t == nil {
		return shortHex(id[:]), ""
	}
	return t.Name, t.Host
}

// shortHex returns the first 8 hex chars of a tenant id; used as a
// fallback when the resolver does not return a tenant.
func shortHex(b []byte) string {
	const hex = "0123456789abcdef"
	if len(b) == 0 {
		return ""
	}
	var out [16]byte
	n := 0
	for i := 0; i < len(b) && n < 16; i++ {
		out[n] = hex[b[i]>>4]
		out[n+1] = hex[b[i]&0x0f]
		n += 2
	}
	return "tenant:" + string(out[:n])
}
