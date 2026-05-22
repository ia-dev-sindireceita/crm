package branding

import (
	"html/template"

	"github.com/pericles-luz/crm/internal/branding"
)

// pageData seeds the full /branding page.
type pageData struct {
	TenantName string
	CSRFMeta   template.HTML
	HXHeaders  template.HTMLAttr
	Preview    previewData
}

// previewData is the form + swatches fragment the upload, override,
// save and revert flows all return (sometimes wrapped in saveData).
// Slots is the ordered list of editable swatches and pairs each slot
// with its label + current hex so the template stays free of helper
// calls.
type previewData struct {
	Slots         []slotData
	PreviewStyle  template.CSS
	SourceLabel   string
	TextOnPrimary string
	PrimaryHex    string
}

type slotData struct {
	Name  string
	Label string
	Hex   string
}

// saveData is previewData plus an OOB-swap fragment that refreshes
// the <style id="tenant-theme"> element on the open page so the user
// sees the new palette without a reload.
type saveData struct {
	Preview    previewData
	ThemeStyle template.CSS
	Message    string
	// CSPNonce carries the per-request CSP nonce (SIN-63275). Empty
	// when csp.Middleware is absent — the swapped <style> still emits
	// the attribute so the browser blocks the inline tag (fail-closed).
	CSPNonce string
}

// errorData is the tiny fragment returned for inline 4xx responses
// (size cap, MIME, WCAG, malformed hex). The status line stays in
// the HTTP response; this fragment carries the user-facing text.
type errorData struct {
	Message string
}

// previewFromPalette projects a domain Palette into the template
// view-model. The style attribute is a pre-rendered template.CSS so
// html/template emits it verbatim into the inline <style> block.
func previewFromPalette(p branding.Palette) previewData {
	return previewData{
		Slots: []slotData{
			{Name: "primary", Label: "Primária", Hex: p.Primary.Hex()},
			{Name: "secondary", Label: "Secundária", Hex: p.Secondary.Hex()},
			{Name: "accent", Label: "Destaque", Hex: p.Accent.Hex()},
			{Name: "foreground", Label: "Texto", Hex: p.Foreground.Hex()},
			{Name: "background", Label: "Fundo", Hex: p.Background.Hex()},
			{Name: "text_on_primary", Label: "Texto sobre primária", Hex: p.TextOnPrimary.Hex()},
		},
		PreviewStyle:  branding.ThemeStyleFromPalette(p),
		SourceLabel:   sourceLabel(p.Source),
		TextOnPrimary: p.TextOnPrimary.Hex(),
		PrimaryHex:    p.Primary.Hex(),
	}
}

func sourceLabel(s branding.PaletteSource) string {
	switch s {
	case branding.PaletteSourceExtracted:
		return "Extraída do logo"
	case branding.PaletteSourceManual:
		return "Override manual"
	case branding.PaletteSourceFallback:
		return "Fallback WCAG (logo não atendia contraste)"
	default:
		return "Padrão neutro"
	}
}

// previewPartialSrc is the form fragment HTMX swaps in after every
// upload / override response. It carries every slot as a hidden input
// (so the next override or save round-trips the full palette) plus a
// visible <input type="color"> the operator can edit.
const previewPartialSrc = `
{{define "branding.previewCard"}}
<section id="branding-preview" class="branding-preview">
  <header class="branding-preview-header">
    <h3>Pré-visualização</h3>
    <p class="branding-preview-source"><small>{{.SourceLabel}}</small></p>
  </header>
  <div class="branding-preview-sample" style="{{.PreviewStyle}}">
    <div class="branding-preview-plate branding-preview-plate--primary"
         style="background:{{.PrimaryHex}};color:{{.TextOnPrimary}};">
      Exemplo de texto sobre a cor primária
    </div>
  </div>
  <form id="branding-form" class="branding-form"
        hx-post="/branding/palette/save"
        hx-target="#branding-preview"
        hx-swap="outerHTML"
        autocomplete="off">
    <fieldset class="branding-swatches">
      <legend>Ajuste manual</legend>
      {{- range .Slots}}
      <label class="branding-swatch">
        <span class="branding-swatch-label">{{.Label}}</span>
        <input class="branding-swatch-input"
               type="color"
               name="{{.Name}}"
               value="{{.Hex}}"
               hx-post="/branding/palette/override"
               hx-trigger="change"
               hx-include="#branding-form"
               hx-vals='{"slot":"{{.Name}}"}'
               hx-target="#branding-preview"
               hx-swap="outerHTML"
               aria-label="{{.Label}}">
        <code class="branding-swatch-hex">{{.Hex}}</code>
      </label>
      {{- end}}
    </fieldset>
    <div class="branding-actions">
      <button type="submit" class="branding-action branding-action--save">Salvar paleta</button>
      <button type="button"
              class="branding-action branding-action--revert"
              hx-post="/branding/palette/revert"
              hx-target="#branding-preview"
              hx-swap="outerHTML"
              hx-confirm="Reverter para o padrão? As cores customizadas serão removidas.">Reverter para padrão</button>
    </div>
  </form>
  <div id="branding-flash" class="branding-flash" aria-live="polite"></div>
</section>
{{end}}
`

// errorPartialSrc is the tiny fragment surfaced for inline 4xx
// responses. HTMX swaps it into #branding-flash; the parent preview
// stays untouched so the operator can fix the value and retry.
const errorPartialSrc = `
{{define "branding.errorFlash"}}
<div id="branding-flash" class="branding-flash branding-flash--error"
     role="alert"
     hx-swap-oob="outerHTML">
  <p>{{.Message}}</p>
</div>
{{end}}
`

// saveSrc wraps previewCard plus an OOB swap that refreshes the
// document-level <style id="tenant-theme"> so the runtime theme
// updates without a page reload (per the AC: "flush de tema via OOB
// swap"). The success message is rendered into the flash slot.
//
// SIN-63275: the OOB `<style>` carries `nonce="{{.CSPNonce}}"` so the
// fragment passes the strict `style-src 'self' 'nonce-…'` policy after
// HTMX swaps it into the live document. Without the nonce, the browser
// silently drops the swapped inline stylesheet.
const saveSrc = `
{{define "branding.saveResponse"}}
{{template "branding.previewCard" .Preview}}
<style id="tenant-theme" nonce="{{.CSPNonce}}" hx-swap-oob="outerHTML">{{.ThemeStyle}}</style>
<div id="branding-flash" class="branding-flash branding-flash--success"
     role="status"
     hx-swap-oob="outerHTML">
  <p>{{.Message}}</p>
</div>
{{end}}
`

// pageSrc is the full /branding page. It carries the layout shell
// (csrf meta, hx-headers, upload form, preview anchor) and embeds
// the previewCard template so the initial render uses the same
// fragment HTMX will later swap.
const pageSrc = `
{{define "branding.page"}}
<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Identidade visual — {{.TenantName}}</title>
  {{.CSRFMeta}}
</head>
<body {{.HXHeaders}}>
  <main class="branding-shell">
    <header class="branding-header">
      <h1>Identidade visual</h1>
      <p>Envie a logo do tenant para gerar uma paleta automática, ajuste as cores
         e salve. As alterações refletem em todas as páginas autenticadas.</p>
    </header>
    <section class="branding-upload">
      <h2>Logo</h2>
      <form id="branding-upload-form"
            class="branding-upload-form"
            hx-post="/branding/logo"
            hx-encoding="multipart/form-data"
            hx-target="#branding-preview"
            hx-swap="outerHTML">
        <label class="branding-upload-label">
          <span>Selecione um PNG ou JPEG (até 2 MB).</span>
          <input type="file" name="logo" accept="image/png,image/jpeg" required>
        </label>
        <button type="submit" class="branding-action branding-action--upload">Enviar logo</button>
      </form>
    </section>
    {{template "branding.previewCard" .Preview}}
  </main>
</body>
</html>
{{end}}
`

// templateSet is the parsed bundle every render reaches into. The
// initial template.Must parse defines four named blocks; each
// exported var resolves the one its handler renders, so the
// {{template "branding.previewCard"}} cross-reference inside the page
// resolves against the same set.
var (
	templateSet = template.Must(template.New("branding").
			Parse(pageSrc + previewPartialSrc + errorPartialSrc + saveSrc))
	pageTmpl    = mustNamed(templateSet, "branding.page")
	previewTmpl = mustNamed(templateSet, "branding.previewCard")
	saveTmpl    = mustNamed(templateSet, "branding.saveResponse")
	errorTmpl   = mustNamed(templateSet, "branding.errorFlash")
)

// mustNamed returns the named block from the shared template set.
// Panicking at init matches the pattern in views/views.go: a missing
// {{define}} is a programmer error caught on first import.
func mustNamed(set *template.Template, name string) *template.Template {
	got := set.Lookup(name)
	if got == nil {
		panic("web/branding: template not found: " + name)
	}
	return got
}
