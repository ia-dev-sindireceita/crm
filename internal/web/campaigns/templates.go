package campaigns

import (
	"html/template"
	"io"
)

// funcs is the shared funcmap every template in this package parses
// against. Registering helpers at parse time avoids the "function not
// defined" panic html/template raises when a template body references
// an unregistered function.
var funcs = template.FuncMap{
	"mkClicksView": mkClicksView,
}

// listLayoutTmpl is the dashboard shell. The table rows live in their
// own partial (listRowsTmpl) so the POST /campaigns response can swap
// the rows back inline without re-rendering the full page.
var listLayoutTmpl = template.Must(template.New("campaigns.list").Funcs(funcs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Campanhas</title>
  {{.CSRFMeta}}
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/tokens.css">
  <link rel="stylesheet" href="/static/css/components.css">
  <link rel="stylesheet" href="/static/css/campaigns.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
  <script src="/static/js/campaigns.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="campaigns-shell" role="main" aria-label="Campanhas de marketing">
    <header class="campaigns-shell__header">
      <h1>Campanhas</h1>
      <nav class="campaigns-shell__actions" aria-label="Ações de campanha">
        <a class="campaigns-shell__new"
           hx-get="/campaigns/new"
           hx-target="#campaigns-table"
           hx-swap="innerHTML"
           href="/campaigns/new">+ nova campanha</a>
      </nav>
    </header>
    <section class="campaigns-table-wrap" aria-live="polite">
      <div id="campaigns-table">
        {{template "rows" .}}
      </div>
    </section>
    <footer class="campaigns-shell__footer">
      <small>Atualizado em {{.GeneratedAt}}</small>
    </footer>
  </main>
</body>
</html>
`))

// listRowsTmpl is the rows partial. POST /campaigns and the page
// shell both render it so the response shape stays identical.
var listRowsTmpl = template.Must(template.New("rows").Funcs(funcs).Parse(`<table class="campaigns-table" role="grid" aria-label="Lista de campanhas">
  <thead>
    <tr>
      <th scope="col">Nome</th>
      <th scope="col">Link público</th>
      <th scope="col" class="num">Cliques</th>
      <th scope="col" class="num">Atribuições</th>
      <th scope="col">Expira em</th>
      <th scope="col">Status</th>
    </tr>
  </thead>
  <tbody>
{{- range .Rows}}
    <tr data-slug="{{.Slug}}" class="{{if .IsExpired}}campaign-row--expired{{end}}">
      <th scope="row">
        <a href="{{.DetailURL}}" hx-get="{{.DetailURL}}" hx-target="#campaigns-table" hx-swap="innerHTML">{{.Name}}</a>
      </th>
      <td>
        <span class="campaign-link" data-link="{{.Link}}">{{.Link}}</span>
        <button type="button" class="campaign-copy" data-link="{{.Link}}" aria-label="Copiar link de {{.Name}}">copiar</button>
      </td>
      <td class="num">{{.Clicks}}</td>
      <td class="num">{{.Attributions}}</td>
      <td>{{.ExpiresLabel}}</td>
      <td><span class="campaign-status campaign-status--{{.Status}}">{{.Status}}</span></td>
    </tr>
{{- else}}
    <tr><td colspan="6" class="campaigns-empty">Nenhuma campanha ainda. Use “+ nova campanha”.</td></tr>
{{- end}}
  </tbody>
</table>
`))

// formLayoutTmpl is the create-campaign form (full-page shell). POST
// submits via HTMX to /campaigns and the rows partial is swapped in
// on success; on 422 the form re-renders with the inline error.
var formLayoutTmpl = template.Must(template.New("campaigns.form").Funcs(funcs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Nova campanha</title>
  {{.CSRFMeta}}
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/tokens.css">
  <link rel="stylesheet" href="/static/css/components.css">
  <link rel="stylesheet" href="/static/css/campaigns.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="campaigns-shell" role="main" aria-label="Nova campanha">
    <header class="campaigns-shell__header">
      <h1>Nova campanha</h1>
      <nav class="campaigns-shell__actions">
        <a href="/campaigns" hx-get="/campaigns" hx-target="body" hx-swap="outerHTML">← voltar</a>
      </nav>
    </header>
    <form class="campaign-form"
          hx-post="/campaigns"
          hx-target="#campaigns-table"
          hx-swap="innerHTML">
      {{- if not .Error.IsZero}}
      <div class="campaign-form__alert" role="alert">{{.Error.Message}}</div>
      {{- end}}
      <label>
        Nome
        <input name="name" type="text" required maxlength="200" value="{{.Input.Name}}"
               aria-invalid="{{if eq .Error.Field "name"}}true{{else}}false{{end}}">
      </label>
      <label>
        Slug
        <input name="slug" type="text" required maxlength="80" value="{{.Input.Slug}}"
               pattern="[a-z0-9-]+"
               aria-invalid="{{if eq .Error.Field "slug"}}true{{else}}false{{end}}">
        <small>Use apenas a-z, 0-9 e hifens. Será exposto em /c/&lt;slug&gt;.</small>
      </label>
      <label>
        URL de destino
        <input name="redirect_url" type="url" required maxlength="1024" value="{{.Input.RedirectURL}}"
               aria-invalid="{{if eq .Error.Field "redirect_url"}}true{{else}}false{{end}}">
      </label>
      <fieldset>
        <legend>UTM (opcional)</legend>
        <label>Source <input name="utm_source" type="text" maxlength="128" value="{{.Input.UTMSource}}"></label>
        <label>Medium <input name="utm_medium" type="text" maxlength="128" value="{{.Input.UTMMedium}}"></label>
        <label>Campaign <input name="utm_campaign" type="text" maxlength="128" value="{{.Input.UTMCampaign}}"></label>
        <label>Term <input name="utm_term" type="text" maxlength="128" value="{{.Input.UTMTerm}}"></label>
        <label>Content <input name="utm_content" type="text" maxlength="128" value="{{.Input.UTMContent}}"></label>
      </fieldset>
      <label>
        Expira em (UTC, opcional)
        <input name="expires_at" type="datetime-local" value="{{.Input.ExpiresRaw}}"
               aria-invalid="{{if eq .Error.Field "expires_at"}}true{{else}}false{{end}}">
      </label>
      <div class="campaign-form__actions">
        <button type="submit">Criar campanha</button>
      </div>
    </form>
  </main>
</body>
</html>
`))

// detailLayoutTmpl is the per-campaign drill-down page. The click
// table mounts at #campaign-clicks and HTMX polls every 10s so newly
// arrived rows surface within the 30s AC budget.
var detailLayoutTmpl = template.Must(template.New("campaigns.detail").Funcs(funcs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Campanha · {{.Row.Name}}</title>
  {{.CSRFMeta}}
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/tokens.css">
  <link rel="stylesheet" href="/static/css/components.css">
  <link rel="stylesheet" href="/static/css/campaigns.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
  <script src="/static/js/campaigns.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="campaigns-shell" role="main" aria-label="Detalhe da campanha">
    <header class="campaigns-shell__header">
      <h1>{{.Row.Name}}</h1>
      <nav class="campaigns-shell__actions">
        <a href="/campaigns" hx-get="/campaigns" hx-target="body" hx-swap="outerHTML">← voltar</a>
      </nav>
    </header>
    <section class="campaign-meta" aria-label="Metadados da campanha">
      <dl>
        <dt>Slug</dt><dd>{{.Row.Slug}}</dd>
        <dt>Link público</dt>
        <dd>
          <span class="campaign-link" data-link="{{.Row.Link}}">{{.Row.Link}}</span>
          <button type="button" class="campaign-copy" data-link="{{.Row.Link}}" aria-label="Copiar link">copiar</button>
        </dd>
        <dt>Expira em</dt><dd>{{.Row.ExpiresLabel}}</dd>
        <dt>Status</dt><dd><span class="campaign-status campaign-status--{{.Row.Status}}">{{.Row.Status}}</span></dd>
      </dl>
    </section>
    <section id="campaign-clicks-section" class="campaign-clicks"
             hx-get="/campaigns/{{.Row.Slug}}/clicks"
             hx-trigger="every 10s"
             hx-target="this"
             hx-swap="innerHTML"
             aria-live="polite">
      {{template "clicks-table" mkClicksView .Row .Clicks}}
    </section>
    <footer class="campaigns-shell__footer">
      <small>Atualizado em {{.GeneratedAt}}</small>
    </footer>
  </main>
</body>
</html>
`))

// clicksTableTmpl renders the click-ledger partial: counter pills
// plus the table. Reused both in the initial detail render (inlined
// via {{template "clicks-table"}}) and the HTMX poll response.
var clicksTableTmpl = template.Must(template.New("clicks-table").Funcs(funcs).Parse(`<header class="campaign-clicks__header">
    <span class="campaign-stat" aria-label="Total de cliques">cliques: <strong>{{.Stats.Clicks}}</strong></span>
    <span class="campaign-stat" aria-label="Atribuições">atribuições: <strong>{{.Stats.Attributions}}</strong></span>
  </header>
  <table class="campaign-clicks__table" role="grid" aria-label="Cliques da campanha">
    <thead>
      <tr>
        <th scope="col">Quando (UTC)</th>
        <th scope="col">Click ID</th>
        <th scope="col">Contato</th>
        <th scope="col">IP</th>
        <th scope="col">User-Agent</th>
        <th scope="col">Referrer</th>
      </tr>
    </thead>
    <tbody>
    {{- range .Clicks}}
      <tr>
        <td>{{.When}}</td>
        <td>{{.ClickID}}</td>
        <td>{{.ContactID}}</td>
        <td>{{.IP}}</td>
        <td>{{.UserAgent}}</td>
        <td>{{.Referrer}}</td>
      </tr>
    {{- else}}
      <tr><td colspan="6" class="campaign-clicks__empty">Nenhum clique registrado ainda.</td></tr>
    {{- end}}
    </tbody>
  </table>
`))

// mkClicksView is a template helper that bundles the per-row stats +
// click rows into the shape clicksTableTmpl consumes. Used inline by
// detailLayoutTmpl so the first render and the HTMX poll response
// share one template.
func mkClicksView(row rowView, clicks []clickRow) clicksTableView {
	return clicksTableView{
		Stats:  statsView{Clicks: row.Clicks, Attributions: row.Attributions},
		Clicks: clicks,
	}
}

func init() {
	// Register partials so the layouts can call {{template "rows"}}
	// and {{template "clicks-table"}} against the same parsed body.
	if _, err := listLayoutTmpl.AddParseTree(listRowsTmpl.Name(), listRowsTmpl.Tree); err != nil {
		panic("web/campaigns: register rows partial: " + err.Error())
	}
	if _, err := detailLayoutTmpl.AddParseTree(clicksTableTmpl.Name(), clicksTableTmpl.Tree); err != nil {
		panic("web/campaigns: register clicks-table partial: " + err.Error())
	}

	// Prime html/template's lazy escaper now, before any concurrent
	// goroutine can race on the first Execute call (web/funnel /
	// web/inbox lessons; reference memory `html/template AddParseTree
	// race (web/inbox)`).
	for _, t := range []*template.Template{
		listRowsTmpl, listLayoutTmpl, formLayoutTmpl, clicksTableTmpl, detailLayoutTmpl,
	} {
		_ = t.Execute(io.Discard, nil)
	}
}
