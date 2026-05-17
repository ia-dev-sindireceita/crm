package master

import (
	"html/template"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
)

// billingPageData drives the layout + panel partial for GET /master/
// tenants/{id}/billing. Identical shape across both templates so an
// HTMX partial swap is byte-identical to a full re-render of the same
// region.
type billingPageData struct {
	TenantID     uuid.UUID
	Subscription SubscriptionRow
	Invoices     []InvoiceRow
	Grants       []GrantRow

	CSRFMeta  template.HTML
	HXHeaders template.HTMLAttr
}

// ledgerPageData drives the layout + panel partial + rows-only
// partial. The rows partial reuses the same shape so the load-more
// swap injects rows that match the surrounding markup.
type ledgerPageData struct {
	TenantID uuid.UUID
	Entries  []LedgerRow

	// PageSize is reflected back into the load-more link so the next
	// fetch sees the same size; otherwise the operator's "show 100"
	// request would silently revert to the default.
	PageSize int

	HasMore        bool
	NextCursorAtRF string
	NextCursorID   uuid.UUID

	CSRFMeta  template.HTML
	HXHeaders template.HTMLAttr
}

// newBillingPageData converts the BillingView aggregate into the
// template view-model. csrfToken is the request-scoped token (must be
// non-empty by the time this is called — the handler 500s otherwise).
func newBillingPageData(view BillingView, csrfToken string) billingPageData {
	return billingPageData{
		TenantID:     view.TenantID,
		Subscription: view.Subscription,
		Invoices:     view.Invoices,
		Grants:       view.Grants,
		CSRFMeta:     csrf.MetaTag(csrfToken),
		HXHeaders:    csrf.HXHeadersAttr(csrfToken),
	}
}

// newLedgerPageData converts the LedgerPage payload into the template
// view-model. csrfToken is the request-scoped CSRF token.
func newLedgerPageData(tenantID uuid.UUID, page LedgerPage, pageSize int, csrfToken string) ledgerPageData {
	data := ledgerPageData{
		TenantID:  tenantID,
		Entries:   page.Entries,
		PageSize:  pageSize,
		HasMore:   page.HasMore,
		CSRFMeta:  csrf.MetaTag(csrfToken),
		HXHeaders: csrf.HXHeadersAttr(csrfToken),
	}
	if page.HasMore {
		data.NextCursorAtRF = page.NextCursorOccurredAt.UTC().Format(time.RFC3339Nano)
		data.NextCursorID = page.NextCursorID
	}
	return data
}

// billingLedgerFuncs are the small helpers shared by the C11
// templates. Reuses formatTime / formatBRL from templates.go where
// possible — both packages live in the same Go package so the funcs
// from templates.go are reachable via the FuncMap below.
var billingLedgerFuncs = template.FuncMap{
	"formatTime":      formatTime,
	"formatBRL":       formatBRL,
	"subStatusLabel":  subscriptionStatusLabel,
	"invoiceLabel":    invoiceStateLabel,
	"int64ToStr":      int64ToStr,
	"grantKindLabel":  grantKindLabel,
	"formatGrantTime": formatGrantTime,
	"ledgerSrcLabel":  ledgerSourceLabel,
	"ledgerKindLabel": ledgerKindLabel,
	"isZeroUUID":      isZeroUUID,
	"hasGrantRef":     hasGrantRef,
	"hasSubRef":       hasSubRef,
}

// ledgerSourceLabel maps the persisted token_ledger.source string onto
// a Portuguese badge. Unknown values render the raw string so the
// table degrades safely.
func ledgerSourceLabel(s string) string {
	switch s {
	case "monthly_alloc":
		return "Alocação mensal"
	case "master_grant":
		return "Master grant"
	case "consumption":
		return "Consumo"
	case "":
		return "—"
	default:
		return s
	}
}

// ledgerKindLabel mirrors ledgerSourceLabel for the kind enum
// (`reserve`/`commit`/`release`/`grant`).
func ledgerKindLabel(k string) string {
	switch k {
	case "reserve":
		return "Reserva"
	case "commit":
		return "Commit"
	case "release":
		return "Release"
	case "grant":
		return "Grant"
	case "":
		return "—"
	default:
		return k
	}
}

// isZeroUUID lets the templates branch on "no link target" without
// importing uuid into the template namespace. uuid.UUID's zero value
// is the all-zero string.
func isZeroUUID(id uuid.UUID) bool { return id == uuid.Nil }

// hasGrantRef returns true when the ledger row links to a master
// grant: source == "master_grant" and the id pair is populated.
func hasGrantRef(row LedgerRow) bool {
	return row.Source == "master_grant" && row.MasterGrantID != uuid.Nil
}

// hasSubRef returns true when the ledger row carries a subscription
// cross-reference (active subscription period at the time the row was
// written).
func hasSubRef(row LedgerRow) bool {
	return row.SubscriptionID != uuid.Nil
}

// billingLayoutTmpl is the full-page shell for GET /master/tenants/
// {id}/billing.
var billingLayoutTmpl = template.Must(template.New("billing.layout").Funcs(billingLedgerFuncs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Master · Histórico de cobrança</title>
  {{.CSRFMeta}}
  <link rel="stylesheet" href="/static/css/master.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="master-shell" role="main" aria-labelledby="master-billing-title">
    <header class="master-shell__header">
      <h1 id="master-billing-title">Histórico de cobrança</h1>
      <p class="master-shell__hint">
        Assinatura atual, invoices e cortesias concedidas para este
        tenant. Esta tela é somente leitura — ações destrutivas vivem
        em "Conceder cortesia" e "Atribuir plano".
      </p>
      <p class="master-shell__crumb">
        <a href="/master/tenants">← Tenants</a>
        <a href="/master/tenants/{{.TenantID}}/ledger">Ver ledger ›</a>
      </p>
    </header>
    {{template "billing_panel" .}}
  </main>
</body>
</html>
`))

// billingPanelTmpl is the panel partial returned for HTMX swaps on
// #billing-panel. It hosts the three sub-panels (subscription /
// invoices / grants).
var billingPanelTmpl = template.Must(template.New("billing_panel").Funcs(billingLedgerFuncs).Parse(`<section id="billing-panel" class="master-billing" aria-label="Histórico de cobrança do tenant">
  <section class="master-billing__subscription" aria-labelledby="billing-sub-title">
    <h2 id="billing-sub-title">Assinatura atual</h2>
    {{if .Subscription.IsEmpty}}
      <p class="master-billing__empty">Tenant sem assinatura ativa.</p>
    {{else}}
      <dl class="master-billing__sub-fields">
        <dt>Plano</dt>
        <dd>
          {{if .Subscription.PlanSlug}}
            <span data-plan-slug="{{.Subscription.PlanSlug}}">{{.Subscription.PlanName}}</span>
            <span class="master-billing__sub-price">{{formatBRL .Subscription.PlanPriceCentsBRL}}/mês</span>
          {{else}}
            <span class="master-billing__unset">—</span>
          {{end}}
        </dd>
        <dt>Status</dt>
        <dd><span data-sub-status="{{.Subscription.Status}}">{{subStatusLabel .Subscription.Status}}</span></dd>
        <dt>Período atual</dt>
        <dd>
          <time datetime="{{.Subscription.CurrentPeriodStart.Format "2006-01-02"}}">{{formatTime .Subscription.CurrentPeriodStart}}</time>
          →
          <time datetime="{{.Subscription.CurrentPeriodEnd.Format "2006-01-02"}}">{{formatTime .Subscription.CurrentPeriodEnd}}</time>
        </dd>
        <dt>Próximo invoice</dt>
        <dd><time datetime="{{.Subscription.NextInvoiceAt.Format "2006-01-02"}}">{{formatTime .Subscription.NextInvoiceAt}}</time></dd>
      </dl>
    {{end}}
  </section>

  <section class="master-billing__invoices" aria-labelledby="billing-inv-title">
    <h2 id="billing-inv-title">Invoices</h2>
    <table class="master-billing__table">
      <caption class="visually-hidden">Invoices ordenados por período mais recente</caption>
      <thead>
        <tr>
          <th scope="col">Período</th>
          <th scope="col">Valor</th>
          <th scope="col">Estado</th>
        </tr>
      </thead>
      <tbody>
        {{if .Invoices}}
          {{range .Invoices}}
          <tr class="master-billing__invoice-row" data-invoice-id="{{.ID}}">
            <td>
              <time datetime="{{.PeriodStart.Format "2006-01-02"}}">{{formatTime .PeriodStart}}</time>
              →
              <time datetime="{{.PeriodEnd.Format "2006-01-02"}}">{{formatTime .PeriodEnd}}</time>
            </td>
            <td>{{formatBRL .AmountCentsBRL}}</td>
            <td><span data-invoice-state="{{.State}}">{{invoiceLabel .State}}</span></td>
          </tr>
          {{end}}
        {{else}}
          <tr class="master-billing__empty-row">
            <td colspan="3">Nenhum invoice emitido para este tenant.</td>
          </tr>
        {{end}}
      </tbody>
    </table>
  </section>

  <section class="master-billing__grants" aria-labelledby="billing-grants-title">
    <h2 id="billing-grants-title">Cortesias concedidas</h2>
    <table class="master-billing__table">
      <caption class="visually-hidden">Cortesias emitidas para este tenant, mais recentes primeiro</caption>
      <thead>
        <tr>
          <th scope="col">Tipo</th>
          <th scope="col">Valor</th>
          <th scope="col">Motivo</th>
          <th scope="col">Concedida em</th>
          <th scope="col">Status</th>
        </tr>
      </thead>
      <tbody>
        {{if .Grants}}
          {{range .Grants}}
          <tr class="master-billing__grant-row" data-grant-id="{{.ID}}">
            <td>{{grantKindLabel .Kind}}</td>
            <td>
              {{if eq (printf "%s" .Kind) "free_subscription_period"}}{{.PeriodDays}} dias
              {{else if eq (printf "%s" .Kind) "extra_tokens"}}{{int64ToStr .Amount}} tokens
              {{else}}—{{end}}
            </td>
            <td><span class="master-billing__reason">{{.Reason}}</span></td>
            <td><time datetime="{{.CreatedAt.Format "2006-01-02T15:04:05Z07:00"}}">{{formatGrantTime .CreatedAt}}</time></td>
            <td>
              {{if .Revoked}}<span data-grant-state="revoked" class="master-billing__pill master-billing__pill--revoked">Revogada
                <time datetime="{{.RevokedAt.Format "2006-01-02T15:04:05Z07:00"}}">({{formatGrantTime .RevokedAt}})</time>
              </span>
              {{else if .Consumed}}<span data-grant-state="consumed" class="master-billing__pill master-billing__pill--consumed">Consumida
                <time datetime="{{.ConsumedAt.Format "2006-01-02T15:04:05Z07:00"}}">({{formatGrantTime .ConsumedAt}})</time>
              </span>
              {{else}}<span data-grant-state="active" class="master-billing__pill master-billing__pill--active">Ativa</span>{{end}}
            </td>
          </tr>
          {{end}}
        {{else}}
          <tr class="master-billing__empty-row">
            <td colspan="5">Nenhuma cortesia emitida para este tenant.</td>
          </tr>
        {{end}}
      </tbody>
    </table>
  </section>
</section>
`))

// ledgerLayoutTmpl is the full-page shell for GET /master/tenants/
// {id}/ledger.
var ledgerLayoutTmpl = template.Must(template.New("ledger.layout").Funcs(billingLedgerFuncs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Master · Ledger de tokens</title>
  {{.CSRFMeta}}
  <link rel="stylesheet" href="/static/css/master.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="master-shell" role="main" aria-labelledby="master-ledger-title">
    <header class="master-shell__header">
      <h1 id="master-ledger-title">Ledger de tokens</h1>
      <p class="master-shell__hint">
        Entradas do <code>token_ledger</code> deste tenant ordenadas por
        data de ocorrência, mais recentes primeiro. Use o botão "Carregar
        mais" para paginar — cada página puxa até {{.PageSize}} linhas.
      </p>
      <p class="master-shell__crumb">
        <a href="/master/tenants">← Tenants</a>
        <a href="/master/tenants/{{.TenantID}}/billing">Ver histórico de cobrança ›</a>
      </p>
    </header>
    {{template "ledger_panel" .}}
  </main>
</body>
</html>
`))

// ledgerPanelTmpl is the panel partial returned for HTMX swaps on
// #ledger-panel (page filter / re-fetch). It wraps the rows partial.
var ledgerPanelTmpl = template.Must(template.New("ledger_panel").Funcs(billingLedgerFuncs).Parse(`<section id="ledger-panel" class="master-ledger" aria-label="Ledger de tokens">
  <table class="master-ledger__table" aria-describedby="master-ledger-title">
    <caption class="visually-hidden">Entradas do ledger paginadas, mais recentes primeiro</caption>
    <thead>
      <tr>
        <th scope="col">Ocorrência</th>
        <th scope="col">Origem</th>
        <th scope="col">Tipo</th>
        <th scope="col">Quantidade</th>
        <th scope="col">Referência</th>
      </tr>
    </thead>
    <tbody id="ledger-rows">
      {{template "ledger_rows" .}}
    </tbody>
  </table>
</section>
`))

// ledgerRowsTmpl is the rows-only partial used both as the body of the
// panel partial AND as the HTMX load-more swap target. Keeping it as a
// distinct template lets the load-more action append rows without
// re-rendering the surrounding chrome.
var ledgerRowsTmpl = template.Must(template.New("ledger_rows").Funcs(billingLedgerFuncs).Parse(`{{if .Entries}}
  {{range .Entries}}
  <tr class="master-ledger__row" data-ledger-id="{{.ID}}" data-ledger-source="{{.Source}}">
    <td>
      <time datetime="{{.OccurredAt.Format "2006-01-02T15:04:05Z07:00"}}">{{formatTime .OccurredAt}}</time>
    </td>
    <td><span data-ledger-source-label="{{.Source}}" class="master-ledger__src master-ledger__src--{{.Source}}">{{ledgerSrcLabel .Source}}</span></td>
    <td><span data-ledger-kind="{{.Kind}}">{{ledgerKindLabel .Kind}}</span></td>
    <td><span class="master-ledger__amount">{{int64ToStr .Amount}}</span></td>
    <td>
      {{if hasGrantRef .}}
        <a class="master-ledger__ref master-ledger__ref--grant"
           href="/master/tenants/{{$.TenantID}}/grants/new#grant-row-{{.MasterGrantID}}"
           data-grant-id="{{.MasterGrantID}}">
          Grant {{.MasterGrantExternalID}}
        </a>
      {{else if hasSubRef .}}
        <span class="master-ledger__ref master-ledger__ref--subscription"
              data-subscription-id="{{.SubscriptionID}}">
          {{if .SubscriptionPlanSlug}}Plano {{.SubscriptionPlanSlug}}{{else}}Assinatura{{end}}
        </span>
      {{else if .ExternalRef}}
        <span class="master-ledger__ref master-ledger__ref--external"><code>{{.ExternalRef}}</code></span>
      {{else}}
        <span class="master-ledger__unset">—</span>
      {{end}}
    </td>
  </tr>
  {{end}}
{{else}}
  <tr class="master-ledger__empty">
    <td colspan="5">Nenhum lançamento encontrado para este tenant.</td>
  </tr>
{{end}}
{{if .HasMore}}
<tr class="master-ledger__more" id="ledger-load-more">
  <td colspan="5">
    <button type="button"
            class="master-ledger__more-btn"
            hx-get="/master/tenants/{{.TenantID}}/ledger?cursor_at={{.NextCursorAtRF}}&cursor_id={{.NextCursorID}}&page_size={{.PageSize}}"
            hx-target="#ledger-rows"
            hx-swap="beforeend"
            hx-headers='{"HX-Target":"ledger-rows"}'
            aria-controls="ledger-rows">
      Carregar mais
    </button>
  </td>
</tr>
{{end}}
`))

func init() {
	// Cross-register so the layout templates can call the panel
	// partial, and the panel templates can call the rows partial.
	if _, err := billingLayoutTmpl.AddParseTree(billingPanelTmpl.Name(), billingPanelTmpl.Tree); err != nil {
		panic("web/master: register billing_panel: " + err.Error())
	}
	if _, err := ledgerPanelTmpl.AddParseTree(ledgerRowsTmpl.Name(), ledgerRowsTmpl.Tree); err != nil {
		panic("web/master: register ledger_rows in ledger_panel: " + err.Error())
	}
	if _, err := ledgerLayoutTmpl.AddParseTree(ledgerPanelTmpl.Name(), ledgerPanelTmpl.Tree); err != nil {
		panic("web/master: register ledger_panel in ledger.layout: " + err.Error())
	}
	if _, err := ledgerLayoutTmpl.AddParseTree(ledgerRowsTmpl.Name(), ledgerRowsTmpl.Tree); err != nil {
		panic("web/master: register ledger_rows in ledger.layout: " + err.Error())
	}
	// Prime html/template's lazy escaper before any concurrent Execute
	// can race on the first call (SIN-62774 regression repro).
	for _, t := range []*template.Template{billingPanelTmpl, billingLayoutTmpl, ledgerRowsTmpl, ledgerPanelTmpl, ledgerLayoutTmpl} {
		_ = t.Execute(io.Discard, billingLedgerPrimingData(t))
	}
}

func billingLedgerPrimingData(t *template.Template) interface{} {
	switch t {
	case billingPanelTmpl, billingLayoutTmpl:
		return billingPageData{}
	default:
		return ledgerPageData{}
	}
}
