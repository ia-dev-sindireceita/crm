package catalog

import (
	"html/template"
	"strings"
	"time"

	"github.com/pericles-luz/crm/internal/catalog"
)

// ResolveSource labels which scope the cascade preview matched (or
// SourceNone when no argument applied). The handler maps the resolver's
// ScopeType into one of these labels so the template stays free of
// catalog package imports beyond the Scope enum.
type ResolveSource string

const (
	// SourceChannel means the most-specific match was the channel anchor.
	SourceChannel ResolveSource = "channel"
	// SourceTeam means the most-specific match was the team anchor.
	SourceTeam ResolveSource = "team"
	// SourceTenant means the only match was the tenant fallback.
	SourceTenant ResolveSource = "tenant"
	// SourceNone means no argument matched. The preview widget renders
	// a "nenhum argumento configurado" badge in this case.
	SourceNone ResolveSource = "none"
)

func sourceFromAnchorType(t catalog.ScopeType) ResolveSource {
	switch t {
	case catalog.ScopeChannel:
		return SourceChannel
	case catalog.ScopeTeam:
		return SourceTeam
	case catalog.ScopeTenant:
		return SourceTenant
	default:
		return SourceNone
	}
}

// pageData drives the /catalog list page.
//
// CSRFMeta and HXHeaders are required because the CSRF middleware on
// the authed group rejects POST/PATCH/DELETE without an X-CSRF-Token
// header. The HTMX swap targets on this page (Apagar, Salvar) issue
// state-changing requests; without hx-headers on <body> they 403.
type pageData struct {
	TenantName  string
	GeneratedAt string
	Rows        []productRow
	// Categories is the sidebar group-count list (SIN-63946).
	Categories []CategoryBucket
	// Filter exposes the active ?q= / ?category= so the template can
	// keep the search box and the active sidebar bucket in sync.
	Filter    listFilter
	CSRFMeta  template.HTML
	HXHeaders template.HTMLAttr
	// TenantThemeStyle carries the per-request runtime theming inline
	// style (SIN-63085).
	TenantThemeStyle template.CSS
	// CSPNonce carries the per-request CSP nonce (SIN-63275). Empty
	// when the csp.Middleware is absent — the layout still emits the
	// attribute so the browser blocks the inline tag (fail-closed).
	CSPNonce string
}

// listPartialData drives the HTMX-swapped list after a mutation.
type listPartialData struct {
	Rows []productRow
	Now  string
}

// productRow is one row in the catalog table.
type productRow struct {
	ID          string
	Name        string
	Description string
	PriceCents  int
	Tags        []string
	Category    string
	UpdatedAt   string
}

// detailData drives the /catalog/{id} detail page. Same CSRF rationale
// as pageData. The fields are only rendered by detailTmpl (full page);
// detailPartialTmpl ignores them because the swap target lives inside
// the already-rendered <body>, whose hx-headers still apply.
type detailData struct {
	Product   productRow
	Arguments []argumentRow
	Preview   previewData
	CSRFMeta  template.HTML
	HXHeaders template.HTMLAttr
	// TenantThemeStyle carries the per-request runtime theming inline
	// style (SIN-63085).
	TenantThemeStyle template.CSS
	// CSPNonce carries the per-request CSP nonce (SIN-63275). Empty
	// when the csp.Middleware is absent — the layout still emits the
	// attribute so the browser blocks the inline tag (fail-closed).
	CSPNonce string
}

// argumentRow is one row in the per-product argument table.
type argumentRow struct {
	ID        string
	ScopeType string
	ScopeID   string
	Text      string
	UpdatedAt string
}

// previewData is the cascade-preview view-model.
type previewData struct {
	Argument  argumentRow
	Source    ResolveSource
	TeamID    string
	ChannelID string
	ProductID string
}

// productFormData drives the new / edit product form.
type productFormData struct {
	Action       string
	Method       string
	IsNew        bool
	Name         string
	Description  string
	PriceCents   int
	TagsRaw      string
	Category     string
	FieldError   string
	ErrorMessage string
}

// argumentFormData drives the new / edit argument form.
type argumentFormData struct {
	Action        string
	Method        string
	IsNew         bool
	ProductID     string
	ArgumentID    string
	ScopeType     string
	ScopeID       string
	Text          string
	AllowedScopes []string
	FieldError    string
	ErrorMessage  string
}

// rowsFromProducts maps the domain slice into the view-model. The
// product.ListByTenant adapter sorts by created_at ASC; we preserve
// that order so the page is deterministic across reloads.
func rowsFromProducts(in []*catalog.Product) []productRow {
	out := make([]productRow, 0, len(in))
	for _, p := range in {
		if p == nil {
			continue
		}
		out = append(out, rowFromProduct(p))
	}
	return out
}

func rowFromProduct(p *catalog.Product) productRow {
	if p == nil {
		return productRow{}
	}
	return productRow{
		ID:          p.ID().String(),
		Name:        p.Name(),
		Description: p.Description(),
		PriceCents:  p.PriceCents(),
		Tags:        p.Tags(),
		Category:    p.Category(),
		UpdatedAt:   p.UpdatedAt().UTC().Format(time.RFC3339),
	}
}

func rowsFromArguments(in []*catalog.ProductArgument) []argumentRow {
	out := make([]argumentRow, 0, len(in))
	for _, a := range in {
		if a == nil {
			continue
		}
		out = append(out, argumentRow{
			ID:        a.ID().String(),
			ScopeType: string(a.Anchor().Type),
			ScopeID:   a.Anchor().ID,
			Text:      a.Text(),
			UpdatedAt: a.UpdatedAt().UTC().Format(time.RFC3339),
		})
	}
	return out
}

func rowFromPreview(a *catalog.ProductArgument) argumentRow {
	if a == nil {
		return argumentRow{}
	}
	return argumentRow{
		ID:        a.ID().String(),
		ScopeType: string(a.Anchor().Type),
		ScopeID:   a.Anchor().ID,
		Text:      a.Text(),
		UpdatedAt: a.UpdatedAt().UTC().Format(time.RFC3339),
	}
}

// formatPrice renders price_cents into a localized BRL string with two
// decimal places. Templates call this so cents never leak into the page
// without a unit label.
func formatPrice(cents int) string {
	whole := cents / 100
	frac := cents % 100
	if frac < 0 {
		frac = -frac
	}
	wholeStr := itoaWithThousandsSep(whole)
	if frac < 10 {
		return "R$ " + wholeStr + ",0" + itoa(frac)
	}
	return "R$ " + wholeStr + "," + itoa(frac)
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = digits[i%10]
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func itoaWithThousandsSep(i int) string {
	s := itoa(i)
	if len(s) <= 3 {
		return s
	}
	neg := s[0] == '-'
	if neg {
		s = s[1:]
	}
	n := len(s)
	out := make([]byte, 0, n+(n-1)/3)
	for i, c := range []byte(s) {
		if i > 0 && (n-i)%3 == 0 {
			out = append(out, '.')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// scopeLabel renders the operator-facing label for a scope type.
func scopeLabel(s string) string {
	switch s {
	case string(catalog.ScopeChannel):
		return "Canal"
	case string(catalog.ScopeTeam):
		return "Equipe"
	case string(catalog.ScopeTenant):
		return "Tenant"
	default:
		return s
	}
}

// sourceLabel renders the operator-facing badge for a preview match.
func sourceLabel(s ResolveSource) string {
	switch s {
	case SourceChannel:
		return "Canal (override mais específico)"
	case SourceTeam:
		return "Equipe (override)"
	case SourceTenant:
		return "Tenant (padrão)"
	case SourceNone:
		return "Nenhum argumento configurado"
	default:
		return string(s)
	}
}

// joinTags renders the tag slice as a comma-separated string. Empty
// slices yield "—" so the table cell is never blank.
func joinTags(tags []string) string {
	if len(tags) == 0 {
		return "—"
	}
	return strings.Join(tags, ", ")
}

// categoryLabel renders a product's category column, falling back to
// the "Sem categoria" badge when the field is empty so the table cell
// stays scannable. Matches the sidebar label for the
// UncategorizedKey bucket.
func categoryLabel(s string) string {
	if strings.TrimSpace(s) == "" {
		return "Sem categoria"
	}
	return s
}

var funcs = template.FuncMap{
	"formatPrice":       formatPrice,
	"scopeLabel":        scopeLabel,
	"sourceLabel":       sourceLabel,
	"joinTags":          joinTags,
	"categoryLabel":     categoryLabel,
	"promptPreviewSeed": promptPreviewSeed,
}

// listPartialSrc is the catalog table partial. It is rendered both by
// the full page and by the post-mutation HTMX response so the swap
// target stays in sync.
const listPartialSrc = `
{{define "catalog.listPartial"}}
<table id="catalog-list" class="catalog-list">
  <thead><tr><th>Nome</th><th>Categoria</th><th>Descrição</th><th>Preço</th><th>Tags</th><th>Atualizado</th><th>Ações</th></tr></thead>
  <tbody>
  {{- range .Rows}}
    <tr>
      <td><a href="/catalog/{{.ID}}">{{.Name}}</a></td>
      <td>{{categoryLabel .Category}}</td>
      <td>{{.Description}}</td>
      <td>{{formatPrice .PriceCents}}</td>
      <td>{{joinTags .Tags}}</td>
      <td><time datetime="{{.UpdatedAt}}">{{.UpdatedAt}}</time></td>
      <td>
        <button type="button" hx-get="/catalog/{{.ID}}/edit" hx-target="#catalog-form" hx-swap="innerHTML">Editar</button>
        <button type="button" hx-delete="/catalog/{{.ID}}" hx-target="#catalog-list" hx-swap="outerHTML" hx-confirm="Apagar este produto?">Apagar</button>
      </td>
    </tr>
  {{- else}}
    <tr><td colspan="7">Nenhum produto cadastrado.</td></tr>
  {{- end}}
  </tbody>
</table>
{{end}}
`

// categorySidebarSrc renders the category-count sidebar plus the
// search box. Both the search box and the bucket links GET /catalog
// with hx-push-url so the operator can deep-link, refresh, and copy a
// filtered view.
const categorySidebarSrc = `
{{define "catalog.sidebar"}}
<aside class="catalog-sidebar" aria-label="Categorias">
  <form class="catalog-sidebar__search" hx-get="/catalog" hx-target="#catalog-list-wrapper" hx-select="#catalog-list" hx-push-url="true" hx-trigger="keyup changed delay:300ms from:input, change from:input">
    <label>Buscar por nome
      <input type="search" name="q" value="{{.Filter.Query}}" maxlength="200" placeholder="ex.: mensalidade">
    </label>
    {{- if .Filter.Category}}<input type="hidden" name="category" value="{{.Filter.Category}}">{{end}}
  </form>
  <nav class="catalog-sidebar__categories" aria-label="Filtrar por categoria">
    <a href="/catalog{{if .Filter.Query}}?q={{.Filter.Query}}{{end}}"
       hx-get="/catalog{{if .Filter.Query}}?q={{.Filter.Query}}{{end}}"
       hx-target="#catalog-list-wrapper" hx-select="#catalog-list" hx-push-url="true"
       {{if eq .Filter.Category ""}}aria-current="page"{{end}}>Todas</a>
    {{- range .Categories}}
    {{- $href := printf "/catalog?category=%s" .Key}}
    {{- if $.Filter.Query}}{{$href = printf "%s&q=%s" $href $.Filter.Query}}{{end}}
    <a href="{{$href}}"
       hx-get="{{$href}}"
       hx-target="#catalog-list-wrapper" hx-select="#catalog-list" hx-push-url="true"
       data-category-key="{{.Key}}"
       {{if eq .Key $.Filter.Category}}aria-current="page"{{end}}>{{.Label}} <span class="catalog-sidebar__count">({{.Count}})</span></a>
    {{- end}}
  </nav>
</aside>
{{end}}
`

// previewPartialSrc is the cascade-preview card. Used by the detail page
// and the standalone preview response.
const previewPartialSrc = `
{{define "catalog.previewCard"}}
<div id="preview" data-source="{{.Source}}">
  <p><strong>{{sourceLabel .Source}}</strong></p>
  {{- if ne (printf "%s" .Source) "none"}}
    <p>Escopo: {{scopeLabel .Argument.ScopeType}} · ID: {{.Argument.ScopeID}}</p>
    <p>{{.Argument.Text}}</p>
  {{- else}}
    <p>Nenhum argumento aplicável para este escopo.</p>
  {{- end}}
</div>
{{end}}
`

// promptPreviewSrc is the live-preview panel that re-renders on every
// keystroke in the argument editor (SIN-63946). The three boxes
// (system / user / argument) are class-tagged so the operator can tell
// at a glance which segment they are editing. The wrapping id stays
// stable so HTMX can swap the entire fragment without remount jitter.
const promptPreviewSrc = `
{{define "catalog.promptPreview"}}
<div id="prompt-preview" class="catalog-prompt-preview" data-product="{{.ProductName}}" data-scope-type="{{.ScopeLabel}}" data-scope-id="{{.ScopeID}}">
  <header class="catalog-prompt-preview__header">
    <strong>Prompt completo (preview)</strong>
    <span>Escopo: {{.ScopeLabel}}{{if .ScopeID}} · ID {{.ScopeID}}{{end}}</span>
  </header>
  {{- range .Segments}}
  <section class="catalog-prompt-preview__segment catalog-prompt-preview__segment--{{.Role}}" data-role="{{.Role}}">
    <span class="catalog-prompt-preview__role">{{.Label}}</span>
    <pre class="catalog-prompt-preview__text">{{.Text}}</pre>
  </section>
  {{- end}}
</div>
{{end}}
`

// promptPreviewSeed is the template helper the argument form calls to
// seed the embedded preview with the operator's current draft, so the
// first render (before HTMX has fired) already reflects the saved text
// rather than an empty placeholder.
func promptPreviewSeed(scopeType, scopeID, text string) PromptPreview {
	return BuildPromptPreview("", scopeType, scopeID, text)
}

// detailPartialSrc is the inner argument table + preview shell.
// Rendered by both the full detail page and the HTMX swap after an
// argument mutation.
const detailPartialSrc = `
{{define "catalog.detailPartial"}}
<section id="catalog-detail">
<h2>Argumentos por escopo</h2>
<table id="argument-list">
  <thead><tr><th>Escopo</th><th>ID do escopo</th><th>Texto</th><th>Atualizado</th><th>Ações</th></tr></thead>
  <tbody>
  {{- range .Arguments}}
    <tr>
      <td>{{scopeLabel .ScopeType}}</td>
      <td>{{.ScopeID}}</td>
      <td>{{.Text}}</td>
      <td><time datetime="{{.UpdatedAt}}">{{.UpdatedAt}}</time></td>
      <td>
        <button type="button" hx-get="/catalog/{{$.Product.ID}}/arguments/{{.ID}}/edit" hx-target="#argument-form" hx-swap="innerHTML">Editar</button>
        <button type="button" hx-delete="/catalog/{{$.Product.ID}}/arguments/{{.ID}}" hx-target="#catalog-detail" hx-swap="outerHTML" hx-confirm="Apagar este argumento?">Apagar</button>
      </td>
    </tr>
  {{- else}}
    <tr><td colspan="5">Nenhum argumento cadastrado.</td></tr>
  {{- end}}
  </tbody>
</table>
<section id="argument-form">
<button type="button" hx-get="/catalog/{{.Product.ID}}/arguments/new" hx-target="#argument-form" hx-swap="innerHTML">Novo argumento</button>
</section>
<section>
  <h2>Cascade preview</h2>
  <form hx-get="/catalog/{{.Product.ID}}/preview" hx-target="#preview" hx-swap="outerHTML">
    <label>team_id <input type="text" name="team_id" value="{{.Preview.TeamID}}" maxlength="128"></label>
    <label>channel_id <input type="text" name="channel_id" value="{{.Preview.ChannelID}}" maxlength="128"></label>
    <button type="submit">Preview</button>
  </form>
  {{template "catalog.previewCard" .Preview}}
</section>
</section>
{{end}}
`

// pageTmpl is the full /catalog page.
var pageTmpl = mustParse("catalog.page", `<!doctype html>
<html lang="pt-BR">
<head>
<meta charset="utf-8">
<title>Catálogo · {{.TenantName}}</title>
{{.CSRFMeta}}
{{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
</head>
<body {{.HXHeaders}}>
<main class="catalog-shell">
<h1>Catálogo</h1>
<p>Tenant: {{.TenantName}} · Atualizado em {{.GeneratedAt}}</p>
<div class="catalog-shell__columns">
{{template "catalog.sidebar" .}}
<section id="catalog-list-wrapper" class="catalog-shell__main">
  {{template "catalog.listPartial" .}}
</section>
</div>
<section id="catalog-form">
  <button type="button" hx-get="/catalog/new" hx-target="#catalog-form" hx-swap="innerHTML">Novo produto</button>
</section>
</main>
</body>
</html>
`)

// listPartialTmpl renders the catalog table after a mutation. HTMX
// swaps the outer <table id="catalog-list"> directly.
var listPartialTmpl = mustParse("catalog.listPartial.invoke", `{{template "catalog.listPartial" .}}`)

// detailTmpl renders the /catalog/{id} page.
var detailTmpl = mustParse("catalog.detail", `<!doctype html>
<html lang="pt-BR">
<head>
<meta charset="utf-8">
<title>{{.Product.Name}} · Catálogo</title>
{{.CSRFMeta}}
{{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
</head>
<body {{.HXHeaders}}>
<main>
<p><a href="/catalog">← Voltar ao catálogo</a></p>
<h1>{{.Product.Name}}</h1>
<dl>
  <dt>Descrição</dt><dd>{{.Product.Description}}</dd>
  <dt>Preço</dt><dd>{{formatPrice .Product.PriceCents}}</dd>
  <dt>Tags</dt><dd>{{joinTags .Product.Tags}}</dd>
  <dt>Atualizado</dt><dd><time datetime="{{.Product.UpdatedAt}}">{{.Product.UpdatedAt}}</time></dd>
</dl>
{{template "catalog.detailPartial" .}}
</main>
</body>
</html>
`)

// detailPartialTmpl is the HTMX-swapped detail surface returned after
// an argument mutation.
var detailPartialTmpl = mustParse("catalog.detailPartial.invoke", `{{template "catalog.detailPartial" .}}`)

// previewTmpl is the standalone preview response.
var previewTmpl = mustParse("catalog.preview.invoke", `{{template "catalog.previewCard" .}}`)

// productFormTmpl renders both the new and edit product form.
var productFormTmpl = mustParse("catalog.productForm", `
<form hx-{{.Method}}="{{.Action}}" hx-target="#catalog-list" hx-swap="outerHTML">
<fieldset>
<legend>{{if .IsNew}}Novo produto{{else}}Editar produto{{end}}</legend>
<label>Nome
<input type="text" name="name" value="{{.Name}}" required maxlength="200">
</label>
{{if eq .FieldError "name"}}<p role="alert">{{.ErrorMessage}}</p>{{end}}
<label>Categoria
<input type="text" name="category" value="{{.Category}}" maxlength="64" placeholder="ex.: Assinaturas">
</label>
{{if eq .FieldError "category"}}<p role="alert">{{.ErrorMessage}}</p>{{end}}
<label>Descrição
<textarea name="description" maxlength="2000">{{.Description}}</textarea>
</label>
{{if eq .FieldError "description"}}<p role="alert">{{.ErrorMessage}}</p>{{end}}
<label>Preço (centavos)
<input type="number" name="price_cents" value="{{.PriceCents}}" min="0" max="1000000000">
</label>
{{if eq .FieldError "price_cents"}}<p role="alert">{{.ErrorMessage}}</p>{{end}}
<label>Tags (separadas por vírgula)
<input type="text" name="tags" value="{{.TagsRaw}}">
</label>
{{if eq .FieldError "tags"}}<p role="alert">{{.ErrorMessage}}</p>{{end}}
<button type="submit">{{if .IsNew}}Criar{{else}}Salvar{{end}}</button>
</fieldset>
</form>
`)

// argumentFormTmpl renders both the new and edit argument form.
// SIN-63946: the form now wraps the editor and the live prompt preview
// in a two-column flex so the operator sees the assembled LLM call
// next to the textarea. The textarea fires
// hx-get="/catalog/{ProductID}/arguments/preview-prompt" on every
// keystroke (300ms debounce) — and `hx-include="closest form"` ships
// the surrounding scope_type + scope_id so the preview reflects the
// pinned anchor in real time.
var argumentFormTmpl = mustParse("catalog.argumentForm", `
<form hx-{{.Method}}="{{.Action}}" hx-target="#catalog-detail" hx-swap="outerHTML" class="catalog-arg-editor">
<div class="catalog-arg-editor__columns">
<fieldset class="catalog-arg-editor__form">
<legend>{{if .IsNew}}Novo argumento{{else}}Editar argumento{{end}}</legend>
<label>Escopo
<select name="scope_type"{{if not .IsNew}} disabled{{end}}>
{{range .AllowedScopes}}
<option value="{{.}}"{{if eq . $.ScopeType}} selected{{end}}>{{scopeLabel .}}</option>
{{end}}
</select>
</label>
{{if eq .FieldError "scope_type"}}<p role="alert">{{.ErrorMessage}}</p>{{end}}
<label>ID do escopo
<input type="text" name="scope_id" value="{{.ScopeID}}"{{if not .IsNew}} readonly{{end}} required maxlength="128">
</label>
{{if eq .FieldError "scope_id"}}<p role="alert">{{.ErrorMessage}}</p>{{end}}
<label>Texto
<textarea name="argument_text" required maxlength="4000"
  hx-get="/catalog/{{.ProductID}}/arguments/preview-prompt"
  hx-trigger="keyup changed delay:300ms, change"
  hx-target="#prompt-preview"
  hx-swap="outerHTML"
  hx-include="closest form">{{.Text}}</textarea>
</label>
{{if eq .FieldError "argument_text"}}<p role="alert">{{.ErrorMessage}}</p>{{end}}
<button type="submit">{{if .IsNew}}Criar{{else}}Salvar{{end}}</button>
</fieldset>
<aside class="catalog-arg-editor__preview" aria-live="polite">
{{template "catalog.promptPreview" promptPreviewSeed .ScopeType .ScopeID .Text}}
</aside>
</div>
</form>
`)

// mustParse builds a template seeded with the shared partials
// (listPartial, previewCard, detailPartial, sidebar, promptPreview)
// plus the supplied body. Every callable template carries the same
// partial set so {{template …}} resolves in the page, the
// post-mutation swap, and the standalone fragment paths.
func mustParse(name, body string) *template.Template {
	t := template.New(name).Funcs(funcs)
	template.Must(t.Parse(previewPartialSrc))
	template.Must(t.Parse(listPartialSrc))
	template.Must(t.Parse(detailPartialSrc))
	template.Must(t.Parse(categorySidebarSrc))
	template.Must(t.Parse(promptPreviewSrc))
	template.Must(t.Parse(body))
	return t
}

// promptPreviewTmpl is the standalone fragment GET
// /catalog/{id}/arguments/preview-prompt swaps into the editor pane.
var promptPreviewTmpl = mustParse("catalog.promptPreview.invoke", `{{template "catalog.promptPreview" .}}`)
