package invoices

import (
	"embed"
	"html/template"
	"io"

	"github.com/pericles-luz/crm/internal/web/shell"
)

//go:embed templates
var templatesFS embed.FS

// listLayoutTmpl is the invoice list page composed with the F1 shell layout.
//
// We return the layout sub-template (t.Lookup("layout")) rather than the
// root "shell.layout" tree so plain tmpl.Execute renders the full page —
// shell.Render (ExecuteTemplate "layout") works on it too since both share
// the same template set. This keeps the SIN-65123 asset-link test, which
// drives tmpl.Execute directly, valid against the shell-composed layout.
var listLayoutTmpl = func() *template.Template {
	t := shell.MustParse(nil, templatesFS, "templates/list.html")
	if _, err := t.AddParseTree(bannerFragmentTmpl.Name(), bannerFragmentTmpl.Tree); err != nil {
		panic("web/billing/invoices: register dunning-banner on list: " + err.Error())
	}
	if _, err := t.AddParseTree(tbodyFragmentTmpl.Name(), tbodyFragmentTmpl.Tree); err != nil {
		panic("web/billing/invoices: register invoices-tbody on list: " + err.Error())
	}
	layout := t.Lookup("layout")
	_ = layout.Execute(io.Discard, listView{}) // prewarm escaper (race fix)
	return layout
}()

// detailLayoutTmpl is the per-invoice page composed with the F1 shell layout.
// See listLayoutTmpl for why we return the "layout" sub-template.
var detailLayoutTmpl = func() *template.Template {
	t := shell.MustParse(nil, templatesFS, "templates/detail.html")
	if _, err := t.AddParseTree(bannerFragmentTmpl.Name(), bannerFragmentTmpl.Tree); err != nil {
		panic("web/billing/invoices: register dunning-banner on detail: " + err.Error())
	}
	if _, err := t.AddParseTree(statusFragmentTmpl.Name(), statusFragmentTmpl.Tree); err != nil {
		panic("web/billing/invoices: register invoice-status on detail: " + err.Error())
	}
	layout := t.Lookup("layout")
	_ = layout.Execute(io.Discard, detailView{}) // prewarm escaper (race fix)
	return layout
}()

// tbodyFragmentTmpl is the HTMX-swapped tbody inner HTML. The list page
// and the filter partial endpoint share the same template body.
var tbodyFragmentTmpl = template.Must(template.New("invoices-tbody").Parse(`{{- range .Rows}}
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
  <tr>
    <td colspan="4">
      <div class="invoices-empty">
        <div class="invoices-empty__icon" aria-hidden="true">📄</div>
        <p class="invoices-empty__title">Nenhuma fatura emitida ainda.</p>
        <p class="invoices-empty__hint">Faturas são geradas no primeiro dia de cada ciclo.{{with .NextBillingDate}} Próxima emissão: {{.}}.{{end}}</p>
      </div>
    </td>
  </tr>
{{- end}}`))

// statusFragmentTmpl is the status-badge partial polled by HTMX on the
// detail page. Omits hx-trigger once the charge reaches a terminal status
// so polling stops cleanly.
var statusFragmentTmpl = template.Must(template.New("invoice-status").Parse(`<span id="invoice-status"
      class="invoice-pix-status invoice-pix-status--{{.Status}}"
      {{- if .PollActive}}
      hx-get="/billing/invoices/{{.InvoiceID}}/status"
      hx-trigger="{{.PollInterval}}"
      hx-swap="outerHTML"
      {{- end}}>{{.Label}}</span>`))

// bannerFragmentTmpl is the standalone dunning-banner partial used by both
// the list/detail pages and the /billing/dunning-banner endpoint. Visible
// banners carry F1 alert classes (.alert--info/warning/danger) for styling
// AND the legacy dunning-banner--{severity} class for backward compat.
var bannerFragmentTmpl = template.Must(template.New("dunning-banner").Parse(`{{- if .Visible -}}
<div id="dunning-banner"
     class="alert {{.AlertClass}} dunning-banner dunning-banner--{{.Severity}}"
     role="alert"
     aria-live="assertive">
  <span class="alert__icon" aria-hidden="true">{{.Icon}}</span>
  <div class="alert__body">
    <strong class="alert__title">{{.Title}}</strong>
    <span class="alert__message">{{.Message}}</span>
    {{- if .HasAction}}
    <a class="alert__action btn btn--sm" href="/billing/invoices">regularizar agora</a>
    {{- else}}
    <a class="alert__action" href="/billing/invoices">ver faturas</a>
    {{- end}}
  </div>
</div>
{{- else -}}
<div id="dunning-banner" class="dunning-banner dunning-banner--hidden" aria-hidden="true"></div>
{{- end -}}`))

func init() {
	// Prime html/template's lazy escaper before any concurrent goroutine
	// can race on the first Execute call (same fix as web/inbox; see memory
	// `html/template AddParseTree race (web/inbox)`).
	for _, t := range []*template.Template{
		bannerFragmentTmpl, statusFragmentTmpl, tbodyFragmentTmpl,
	} {
		_ = t.Execute(io.Discard, nil)
	}
}
