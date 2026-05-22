package invoices

import (
	"html/template"
	"io"
)

// listLayoutTmpl is the invoice list page (full-page shell). The
// dunning banner is rendered inline at the top; the table partial
// follows. Both pieces are partials so the page and the partial
// endpoints share one template body.
var listLayoutTmpl = template.Must(template.New("billing.invoices.list").Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Faturas</title>
  {{.CSRFMeta}}
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/billing-invoices.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
  <script src="/static/js/billing-invoices.js" defer></script>
</head>
<body {{.HXHeaders}}>
  {{template "dunning-banner" .Banner}}
  <main class="invoices-shell" role="main" aria-label="Faturas">
    <header class="invoices-shell__header">
      <h1>Faturas</h1>
    </header>
    <section class="invoices-table-wrap" aria-live="polite">
      <table class="invoices-table" role="grid" aria-label="Lista de faturas">
        <thead>
          <tr>
            <th scope="col">Período</th>
            <th scope="col" class="num">Valor</th>
            <th scope="col">Status</th>
            <th scope="col">Detalhe</th>
          </tr>
        </thead>
        <tbody>
        {{- range .Rows}}
          <tr data-invoice="{{.ID}}" class="invoice-row invoice-row--{{.State}}">
            <th scope="row">{{.Period}}</th>
            <td class="num">{{.Amount}}</td>
            <td><span class="invoice-status invoice-status--{{.State}}">{{.StateLabel}}</span></td>
            <td>
              <a href="{{.DetailURL}}"
                 hx-get="{{.DetailURL}}"
                 hx-target="body"
                 hx-swap="outerHTML"
                 hx-push-url="true">abrir</a>
            </td>
          </tr>
        {{- else}}
          <tr><td colspan="4" class="invoices-empty">Nenhuma fatura emitida ainda.</td></tr>
        {{- end}}
        </tbody>
      </table>
    </section>
    <footer class="invoices-shell__footer">
      <small>Atualizado em {{.GeneratedAt}}</small>
    </footer>
  </main>
</body>
</html>
`))

// detailLayoutTmpl is the per-invoice page. The QR + copia-e-cola
// block renders only when the PIX charge is present; otherwise a
// placeholder explains the charge is being generated. The status
// badge mounts at #invoice-status and polls /billing/invoices/{id}/status
// every 10s while pending; the badge omits hx-trigger once the charge
// reaches a terminal status (paid/expired/cancelled).
var detailLayoutTmpl = template.Must(template.New("billing.invoices.detail").Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Fatura · {{.Invoice.Period}}</title>
  {{.CSRFMeta}}
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/billing-invoices.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
  <script src="/static/js/billing-invoices.js" defer></script>
</head>
<body {{.HXHeaders}}>
  {{template "dunning-banner" .Banner}}
  <main class="invoices-shell" role="main" aria-label="Detalhe da fatura">
    <header class="invoices-shell__header">
      <h1>Fatura {{.Invoice.Period}}</h1>
      <nav class="invoices-shell__actions">
        <a href="/billing/invoices" hx-get="/billing/invoices" hx-target="body" hx-swap="outerHTML">← voltar</a>
      </nav>
    </header>

    <section class="invoice-meta" aria-label="Dados da fatura">
      <dl>
        <dt>Período</dt><dd>{{.Invoice.Period}}</dd>
        <dt>Valor</dt><dd>{{.Invoice.Amount}}</dd>
        <dt>Status da fatura</dt>
        <dd><span class="invoice-status invoice-status--{{.Invoice.State}}">{{.Invoice.StateLabel}}</span></dd>
      </dl>
    </section>

    <section class="invoice-pix" aria-label="Cobrança PIX">
      <header class="invoice-pix__header">
        <h2>Pague via PIX</h2>
        {{template "invoice-status" .Status}}
      </header>
      {{- if .Charge.HasCharge}}
      <div class="invoice-pix__qr">
        <img src="{{.Charge.QRDataURI}}" alt="QR code PIX para pagamento" width="240" height="240">
      </div>
      <div class="invoice-pix__copy">
        <label for="invoice-copy-paste">PIX copia-e-cola</label>
        <textarea id="invoice-copy-paste" readonly rows="3" aria-readonly="true">{{.Charge.CopyPaste}}</textarea>
        <button type="button"
                class="invoice-pix__copy-btn"
                data-copy-target="#invoice-copy-paste"
                aria-label="Copiar código PIX">copiar</button>
      </div>
      <p class="invoice-pix__expiry">Expira em {{.Charge.ExpiresAt}}.</p>
      <div class="invoice-pix__check">
        <button type="button"
                hx-get="/billing/invoices/{{.Invoice.ID}}/status"
                hx-target="#invoice-status"
                hx-swap="outerHTML">verificar pagamento</button>
      </div>
      {{- else}}
      <p class="invoice-pix__pending">Cobrança PIX em processamento. Atualize em alguns instantes.</p>
      <div class="invoice-pix__check">
        <button type="button"
                hx-get="/billing/invoices/{{.Invoice.ID}}/status"
                hx-target="#invoice-status"
                hx-swap="outerHTML">verificar pagamento</button>
      </div>
      {{- end}}
    </section>

    <footer class="invoices-shell__footer">
      <small>Atualizado em {{.GeneratedAt}}</small>
    </footer>
  </main>
</body>
</html>
`))

// statusFragmentTmpl is the status-badge partial. While the charge
// is pending the partial carries hx-trigger so HTMX re-fetches it
// every 10s; once terminal the trigger is omitted so polling stops
// cleanly (AC #3).
var statusFragmentTmpl = template.Must(template.New("invoice-status").Parse(`<span id="invoice-status"
      class="invoice-pix-status invoice-pix-status--{{.Status}}"
      {{- if .PollActive}}
      hx-get="/billing/invoices/{{.InvoiceID}}/status"
      hx-trigger="{{.PollInterval}}"
      hx-swap="outerHTML"
      {{- end}}>{{.Label}}</span>`))

// bannerFragmentTmpl is the standalone dunning-banner partial. An
// invisible banner renders as an empty <div> so HTMX swaps still
// have a target node.
var bannerFragmentTmpl = template.Must(template.New("dunning-banner").Parse(`{{- if .Visible -}}
<div id="dunning-banner"
     class="dunning-banner dunning-banner--{{.Severity}}"
     role="alert"
     aria-live="assertive">
  <strong class="dunning-banner__title">{{.Title}}</strong>
  <span class="dunning-banner__message">{{.Message}}</span>
  <a class="dunning-banner__cta" href="/billing/invoices">ver faturas</a>
</div>
{{- else -}}
<div id="dunning-banner" class="dunning-banner dunning-banner--hidden" aria-hidden="true"></div>
{{- end -}}`))

func init() {
	// Register the dunning-banner and invoice-status partials on
	// every layout that references them via {{template "..."}}. Keeps
	// the layouts and the partial endpoints rendering the same body.
	for _, host := range []*template.Template{listLayoutTmpl, detailLayoutTmpl} {
		if _, err := host.AddParseTree(bannerFragmentTmpl.Name(), bannerFragmentTmpl.Tree); err != nil {
			panic("web/billing/invoices: register dunning-banner partial: " + err.Error())
		}
	}
	if _, err := detailLayoutTmpl.AddParseTree(statusFragmentTmpl.Name(), statusFragmentTmpl.Tree); err != nil {
		panic("web/billing/invoices: register invoice-status partial: " + err.Error())
	}

	// Prime html/template's lazy escaper before any concurrent
	// goroutine can race on the first Execute call (web/inbox lesson;
	// see memory `html/template AddParseTree race (web/inbox)`).
	for _, t := range []*template.Template{
		bannerFragmentTmpl, statusFragmentTmpl, listLayoutTmpl, detailLayoutTmpl,
	} {
		_ = t.Execute(io.Discard, nil)
	}
}
