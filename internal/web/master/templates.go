package master

import (
	"html/template"
	"io"
	"time"

	"github.com/pericles-luz/crm/internal/billing"
	"github.com/pericles-luz/crm/internal/web/shell"
)

// pageData drives the full layout AND the table-only partial. The
// list view, the post-create swap, and the validation re-render all
// pass the same shape so the rendered HTML is consistent across them.
type pageData struct {
	Tenants    []TenantRow
	Page       int
	PageSize   int
	TotalCount int
	TotalPages int
	Plans      []billing.Plan

	// Flash is the success message rendered above the table after a
	// non-idempotent action; FormError is the inline error rendered
	// next to the create form. Only one of the two is set at a time.
	Flash     string
	FormError string

	CSRFMeta  template.HTML
	HXHeaders template.HTMLAttr
	CSRFInput template.HTML

	// TenantThemeStyle carries the per-request runtime theming inline
	// style (SIN-63085); the layout's <head> renders it via the
	// {{with}} slot so a tenant with a custom palette sees its colours
	// applied across the master shell.
	TenantThemeStyle template.CSS
	// CSPNonce carries the per-request CSP nonce (SIN-63275). Empty
	// when csp.Middleware is absent — the template still emits the
	// attribute so the browser blocks the inline tag (fail-closed).
	CSPNonce string

	// ActiveImpersonation is the SIN-63956 banner view-model. Non-nil
	// → master shell renders the sticky red banner (spec §2). Nil →
	// banner subtemplate evaluates to empty output. Populated from
	// middleware.ActiveImpersonation by the handler before rendering.
	ActiveImpersonation *shell.ImpersonationContext
}

// rowData drives the per-row partial returned by PATCH /master/
// tenants/{id}/plan. Keeping it separate from pageData lets the row
// partial swap in place via hx-swap=outerHTML without re-rendering the
// surrounding table chrome.
type rowData struct {
	Row       TenantRow
	Plans     []billing.Plan
	CSRFInput template.HTML
}

// templateFuncs is the small helper set used by every template. All
// formatting choices live here so the template source stays declarative.
// rowContext must be in the initial Funcs map because the parser needs
// to recognise the name during Parse — adding it post-parse via Funcs
// is too late in html/template.
var templateFuncs = template.FuncMap{
	"formatTime":                  formatTime,
	"formatBRL":                   formatBRL,
	"subStatusLabel":              subscriptionStatusLabel,
	"invoiceLabel":                invoiceStateLabel,
	"prevPage":                    prevPage,
	"nextPage":                    nextPage,
	"safeFilterParam":             safeFilterParam,
	"rowContext":                  rowContext,
	"formatImpersonationISO":      formatImpersonationISO,
	"truncateImpersonationReason": truncateImpersonationReason,
	"csrfHiddenForToken":          csrfHiddenForToken,
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

func formatBRL(cents int) string {
	if cents == 0 {
		return "R$ 0,00"
	}
	reais := cents / 100
	cs := cents % 100
	if cs < 0 {
		cs = -cs
	}
	return "R$ " + intToStr(reais) + "," + padCents(cs)
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func padCents(c int) string {
	if c < 10 {
		return "0" + intToStr(c)
	}
	return intToStr(c)
}

// subscriptionStatusLabel maps the persisted Subscription status onto
// a Portuguese label. Unknown values render the raw string so the
// table degrades safely.
func subscriptionStatusLabel(s string) string {
	switch s {
	case "active":
		return "Ativo"
	case "cancelled":
		return "Cancelado"
	case "":
		return "Sem assinatura"
	default:
		return s
	}
}

// invoiceStateLabel mirrors subscriptionStatusLabel for the most-recent
// invoice column.
func invoiceStateLabel(s string) string {
	switch s {
	case "pending":
		return "Pendente"
	case "paid":
		return "Pago"
	case "cancelled_by_master":
		return "Cancelado"
	case "":
		return "—"
	default:
		return s
	}
}

func prevPage(p int) int {
	if p <= 1 {
		return 1
	}
	return p - 1
}

func nextPage(p, total int) int {
	if p >= total {
		return total
	}
	return p + 1
}

// safeFilterParam encodes the filter slug for embedding inside an
// href query string. The template's URL-attribute autoescape already
// handles attribute quoting; this helper avoids a leading "&" when
// the filter is empty.
func safeFilterParam(plan string) string {
	if plan == "" {
		return ""
	}
	return "&plan=" + plan
}

// masterLayoutTmpl is the full page shell. The tenants table renders
// inside #tenants-table so the POST/PATCH responses can target that
// region via hx-target / hx-swap=outerHTML.
var masterLayoutTmpl = template.Must(template.New("master.layout").Funcs(templateFuncs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Master · Tenants</title>
  {{.CSRFMeta}}
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/master.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
</head>
<body {{.HXHeaders}}{{with .ActiveImpersonation}} data-impersonating="true"{{end}}>
  {{template "shell_impersonation_banner" .}}
  {{template "shell_audit_feed_chip" .}}
  <main class="master-shell" role="main" aria-labelledby="master-tenants-title">
    <header class="master-shell__header">
      <h1 id="master-tenants-title">Tenants</h1>
      <p class="master-shell__hint">
        Lista de todos os tenants atendidos pela plataforma. Use o
        formulário abaixo para criar um novo tenant e atribuir um plano
        inicial. As ações desta página são auditadas.
      </p>
    </header>
    {{template "tenants_table" .}}
  </main>
</body>
</html>
`))

// tenantsTableTmpl is the partial returned for HTMX swaps on
// #tenants-table. It also embeds the create form so a failed
// validation re-renders the form with the inline error in one round
// trip.
var tenantsTableTmpl = template.Must(template.New("tenants_table").Funcs(templateFuncs).Parse(`<section id="tenants-table" class="master-tenants" aria-label="Lista de tenants">
  {{if .Flash}}
  <div class="master-tenants__flash" role="status">{{.Flash}}</div>
  {{end}}
  <form class="master-tenants__filter"
        method="get"
        action="/master/tenants"
        hx-get="/master/tenants"
        hx-target="#tenants-table"
        hx-swap="outerHTML"
        aria-label="Filtrar por plano">
    <label for="plan-filter">Plano</label>
    <select id="plan-filter" name="plan">
      <option value="">Todos os planos</option>
      {{range .Plans}}
      <option value="{{.Slug}}">{{.Name}}</option>
      {{end}}
    </select>
    <button type="submit" class="master-tenants__filter-submit">Filtrar</button>
  </form>

  <form class="master-tenants__create"
        method="post"
        action="/master/tenants"
        hx-post="/master/tenants"
        hx-target="#tenants-table"
        hx-swap="outerHTML"
        aria-labelledby="create-tenant-title">
    <h2 id="create-tenant-title">Criar tenant</h2>
    {{if .FormError}}
    <p class="master-tenants__form-error" role="alert">{{.FormError}}</p>
    {{end}}
    {{.CSRFInput}}
    <div class="master-tenants__field">
      <label for="tenant-name">Nome</label>
      <input id="tenant-name" name="name" type="text" required autocomplete="off">
    </div>
    <div class="master-tenants__field">
      <label for="tenant-host">Host</label>
      <input id="tenant-host" name="host" type="text" required autocomplete="off"
             placeholder="acme.crm.local">
    </div>
    <div class="master-tenants__field">
      <label for="tenant-plan">Plano inicial</label>
      <select id="tenant-plan" name="plan_slug">
        <option value="">Sem plano</option>
        {{range .Plans}}
        <option value="{{.Slug}}">{{.Name}} ({{formatBRL .PriceCentsBRL}}/mês)</option>
        {{end}}
      </select>
    </div>
    <div class="master-tenants__field">
      <label for="tenant-courtesy">Tokens de cortesia (opcional)</label>
      <input id="tenant-courtesy" name="courtesy_tokens" type="number" min="0" step="1" value="0">
    </div>
    <button type="submit" class="master-tenants__create-submit">Criar tenant</button>
  </form>

  <table class="master-tenants__table" aria-describedby="master-tenants-title">
    <caption class="visually-hidden">Tenants cadastrados — página {{.Page}} de {{.TotalPages}}</caption>
    <thead>
      <tr>
        <th scope="col">Nome</th>
        <th scope="col">Host</th>
        <th scope="col">Plano</th>
        <th scope="col">Status</th>
        <th scope="col">Último invoice</th>
        <th scope="col">Ações</th>
      </tr>
    </thead>
    <tbody>
      {{if .Tenants}}
        {{range .Tenants}}
        {{template "tenant_row" (rowContext . $.Plans $.CSRFInput)}}
        {{end}}
      {{else}}
        <tr class="master-tenants__empty">
          <td colspan="6">Nenhum tenant encontrado.</td>
        </tr>
      {{end}}
    </tbody>
  </table>

  {{if gt .TotalPages 1}}
  <nav class="master-tenants__pager" aria-label="Paginação">
    <a href="/master/tenants?page={{prevPage .Page}}{{safeFilterParam ""}}"
       hx-get="/master/tenants?page={{prevPage .Page}}{{safeFilterParam ""}}"
       hx-target="#tenants-table"
       hx-swap="outerHTML"
       aria-label="Página anterior">‹ Anterior</a>
    <span aria-live="polite">Página {{.Page}} de {{.TotalPages}}</span>
    <a href="/master/tenants?page={{nextPage .Page .TotalPages}}{{safeFilterParam ""}}"
       hx-get="/master/tenants?page={{nextPage .Page .TotalPages}}{{safeFilterParam ""}}"
       hx-target="#tenants-table"
       hx-swap="outerHTML"
       aria-label="Próxima página">Próxima ›</a>
  </nav>
  {{end}}
</section>
`))

// tenantDetailData drives GET /master/tenants/{id} (spec §9.5). The
// page hosts the impersonate trigger; ShowReasonModal switches between
// the static page and the inline reason-modal view (no JS required so
// the noscript pass works — spec §8.2).
type tenantDetailData struct {
	Tenant TenantRow

	CSRFInput template.HTML
	CSRFMeta  template.HTML
	HXHeaders template.HTMLAttr

	TenantThemeStyle template.CSS
	CSPNonce         string

	ActiveImpersonation *shell.ImpersonationContext

	// ShowReasonModal flips the page from the static "Impersonar
	// tenant" CTA to the inline reason-collection form. Toggled by
	// the `?impersonate=ask` query param; clicking Cancel returns to
	// the static page without a JS handler.
	ShowReasonModal bool
}

// tenantDetailLayoutTmpl is the full-page shell for GET /master/tenants/
// {id}. The page is server-rendered + noscript-safe so a JS-disabled
// operator can still trigger impersonation (spec §10.2 + §10.7).
var tenantDetailLayoutTmpl = template.Must(template.New("tenant_detail.layout").Funcs(templateFuncs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Master · Tenant {{.Tenant.Name}}</title>
  {{.CSRFMeta}}
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/master.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
</head>
<body {{.HXHeaders}}{{with .ActiveImpersonation}} data-impersonating="true"{{end}}>
  {{template "shell_impersonation_banner" .}}
  {{template "shell_audit_feed_chip" .}}
  <main class="master-shell" role="main" aria-labelledby="master-tenant-detail-title">
    <header class="master-shell__header">
      <p class="master-shell__crumb">
        <a class="master-tenants__crumb" href="/master/tenants">← Tenants</a>
      </p>
      <h1 id="master-tenant-detail-title">{{.Tenant.Name}}</h1>
      <p class="master-shell__hint">
        <code>{{.Tenant.Host}}</code> · plano
        {{if .Tenant.PlanSlug}}<strong>{{.Tenant.PlanName}}</strong>{{else}}—{{end}} ·
        assinatura {{subStatusLabel .Tenant.SubscriptionStatus}}
      </p>
    </header>

    <section class="master-tenant-detail" aria-label="Ações do tenant">
      <ul class="master-tenant-detail__links">
        <li><a class="master-tenants__action-link" href="/master/tenants/{{.Tenant.ID}}/billing">Histórico de cobrança</a></li>
        <li><a class="master-tenants__action-link" href="/master/tenants/{{.Tenant.ID}}/ledger">Ledger de tokens</a></li>
        <li><a class="master-tenants__action-link" href="/master/tenants/{{.Tenant.ID}}/grants/new">Conceder cortesia</a></li>
        <li><a class="master-tenants__action-link" href="/master/grants/requests">Solicitações 4-eyes</a></li>
      </ul>

      {{if .ShowReasonModal}}
      <section class="master-impersonation-modal"
               role="dialog"
               aria-modal="false"
               aria-labelledby="impersonate-modal-title"
               data-impersonate-modal="true">
        <h2 id="impersonate-modal-title" class="master-impersonation-modal__title">
          Impersonar tenant {{.Tenant.Name}}
        </h2>
        <p>
          Esta ação inicia uma janela de impersonação de 15 minutos.
          O motivo é registrado no <code>audit_log_security</code> e
          permanece visível em todas as rotas até você sair.
        </p>
        <form method="post"
              action="/master/tenants/{{.Tenant.ID}}/impersonate"
              data-impersonate-form="true">
          {{.CSRFInput}}
          <div class="master-impersonation-modal__field">
            <label for="impersonate-reason">
              Motivo da impersonação (registrado no audit log)
            </label>
            <textarea id="impersonate-reason"
                      name="reason"
                      minlength="8"
                      maxlength="500"
                      required
                      autofocus
                      placeholder="Ex.: ticket #11324 — investigar erro 500 em /campaigns"></textarea>
          </div>
          <div class="master-impersonation-modal__actions">
            <a class="master-impersonation-modal__cancel"
               href="/master/tenants/{{.Tenant.ID}}"
               data-impersonate-cancel="true"
               tabindex="0">Cancelar</a>
            <button type="submit"
                    class="master-impersonation-modal__submit"
                    data-impersonate-confirm="true">
              Iniciar impersonação
            </button>
          </div>
        </form>
      </section>
      {{else}}
      <p class="master-tenant-detail__impersonate-cta">
        <a class="master-impersonation-trigger"
           href="/master/tenants/{{.Tenant.ID}}?impersonate=ask"
           data-impersonate-trigger="true">
          Impersonar tenant ▶
        </a>
      </p>
      {{end}}
    </section>
  </main>
</body>
</html>
`))

// tenantRowTmpl is the row partial returned by PATCH /master/tenants/
// {id}/plan. hx-swap=outerHTML on the parent <tr> swaps the whole row
// without disturbing the table chrome.
var tenantRowTmpl = template.Must(template.New("tenant_row").Funcs(templateFuncs).Parse(`<tr class="master-tenants__row" data-tenant-id="{{.Row.ID}}" id="tenant-row-{{.Row.ID}}">
  <td>{{.Row.Name}}</td>
  <td><code>{{.Row.Host}}</code></td>
  <td>{{if .Row.PlanSlug}}<span data-plan-slug="{{.Row.PlanSlug}}">{{.Row.PlanName}}</span>{{else}}<span class="master-tenants__unset">—</span>{{end}}</td>
  <td>{{subStatusLabel .Row.SubscriptionStatus}}</td>
  <td>
    {{if .Row.LastInvoiceState}}
      <span data-invoice-state="{{.Row.LastInvoiceState}}">{{invoiceLabel .Row.LastInvoiceState}}</span>
      <time datetime="{{.Row.LastInvoiceUpdatedAt.Format "2006-01-02T15:04:05Z07:00"}}">{{formatTime .Row.LastInvoiceUpdatedAt}}</time>
    {{else}}
      <span class="master-tenants__unset">—</span>
    {{end}}
  </td>
  <td>
    <form class="master-tenants__row-form"
          method="post"
          action="/master/tenants/{{.Row.ID}}/plan"
          hx-patch="/master/tenants/{{.Row.ID}}/plan"
          hx-target="#tenant-row-{{.Row.ID}}"
          hx-swap="outerHTML"
          aria-label="Atribuir plano ao tenant {{.Row.Name}}">
      {{.CSRFInput}}
      <label class="visually-hidden" for="row-plan-{{.Row.ID}}">Plano</label>
      <select id="row-plan-{{.Row.ID}}" name="plan_slug" required>
        <option value="">Selecione…</option>
        {{range .Plans}}
        <option value="{{.Slug}}" {{if eq .Slug $.Row.PlanSlug}}selected{{end}}>{{.Name}}</option>
        {{end}}
      </select>
      <button type="submit">Atribuir</button>
    </form>
  </td>
</tr>
`))

// rowContext is a template-side adapter that bundles a TenantRow with
// the page-level plan catalogue + CSRF input so the tenant_row partial
// can be invoked from inside a {{range}} on the parent template.
func rowContext(row TenantRow, plans []billing.Plan, csrfInput template.HTML) rowData {
	return rowData{Row: row, Plans: plans, CSRFInput: csrfInput}
}

func init() {
	// Cross-register so the layout / table can call {{template
	// "tenant_row" …}} from inside a {{range}}, and so the layout can
	// call {{template "tenants_table" …}}. Errors here are programmer
	// errors — surface at process start.
	for _, child := range []*template.Template{tenantsTableTmpl, tenantRowTmpl} {
		if _, err := masterLayoutTmpl.AddParseTree(child.Name(), child.Tree); err != nil {
			panic("web/master: register " + child.Name() + ": " + err.Error())
		}
	}
	if _, err := tenantsTableTmpl.AddParseTree(tenantRowTmpl.Name(), tenantRowTmpl.Tree); err != nil {
		panic("web/master: register tenant_row in tenants_table: " + err.Error())
	}
	registerImpersonationBanner(masterLayoutTmpl, tenantDetailLayoutTmpl)
	// Prime html/template's lazy escaper on every template before any
	// concurrent Execute can race on the first call (SIN-62774
	// regression repro).
	for _, t := range []*template.Template{tenantRowTmpl, tenantsTableTmpl, masterLayoutTmpl, tenantDetailLayoutTmpl} {
		_ = t.Execute(io.Discard, primingData(t))
	}
}

// registerImpersonationBanner cross-installs the
// "shell_impersonation_banner" + "shell_audit_feed_chip" partials onto
// every master layout so the {{template "shell_impersonation_banner" .}}
// invocation resolves at render time. Centralised so adding a new
// master layout in the future does not silently lose the banner.
func registerImpersonationBanner(layouts ...*template.Template) {
	for _, layout := range layouts {
		for _, name := range []string{"shell_impersonation_banner", "shell_audit_feed_chip"} {
			tmpl := impersonationBannerTmpl.Lookup(name)
			if tmpl == nil {
				panic("web/master: impersonation banner subtemplate missing: " + name)
			}
			if _, err := layout.AddParseTree(name, tmpl.Tree); err != nil {
				panic("web/master: register " + name + " in " + layout.Name() + ": " + err.Error())
			}
		}
	}
}

// primingData returns the smallest non-nil view-model that satisfies
// each template's `.` lookups during the init() priming pass. Real
// rendering goes through buildPageData / rowData built in handlers.go.
func primingData(t *template.Template) interface{} {
	switch t {
	case tenantRowTmpl:
		return rowData{Row: TenantRow{}, Plans: nil, CSRFInput: ""}
	case tenantDetailLayoutTmpl:
		return tenantDetailData{}
	default:
		return pageData{}
	}
}
