package contacts

import (
	"html/template"
	"io"
	"time"

	"github.com/pericles-luz/crm/internal/contacts"
)

// templateFuncs are the small helper set the templates use. Keeping
// formatting in funcs (rather than inside the handler) keeps the
// template source declarative.
var templateFuncs = template.FuncMap{
	"linkReasonLabel": linkReasonLabel,
	"formatTime":      formatTime,
}

// linkReasonLabel maps the LinkReason enum onto a Portuguese label.
// Unknown reasons render the raw string so the panel degrades safely
// rather than dropping the row from the DOM.
func linkReasonLabel(r contacts.LinkReason) string {
	switch r {
	case contacts.LinkReasonPhone:
		return "Telefone"
	case contacts.LinkReasonEmail:
		return "E-mail"
	case contacts.LinkReasonExternalID:
		return "ID externo"
	case contacts.LinkReasonManual:
		return "Manual"
	default:
		return string(r)
	}
}

// formatTime renders the link timestamp in ISO-8601-ish local form.
// Server-rendered so the panel is readable without JS.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// contactLayoutTmpl is the full-page shell. The identity panel renders
// inside #identity-panel so the POST /contacts/identity/split fragment
// can target #identity-panel via hx-swap=outerHTML.
var contactLayoutTmpl = template.Must(template.New("contact.layout").Funcs(templateFuncs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Identidade do contato</title>
  {{.CSRFMeta}}
  <link rel="stylesheet" href="/static/css/contacts.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="contact-shell" role="main">
    <header class="contact-shell__header">
      <h1>Identidade do contato</h1>
      <p class="contact-shell__hint">
        Esta tela lista todos os contatos mesclados na mesma identidade.
        Use "Separar este contato" se um merge automático foi incorreto —
        a operação é destrutiva: o merge automático é desfeito e o contato
        passa a ter uma identidade própria.
      </p>
    </header>
    {{template "identity_panel" .Panel}}
  </main>
</body>
</html>
`))

// identityPanelTmpl is the swap unit: one row per IdentityLink. The
// "Separar este contato" button POSTs to /contacts/identity/split with
// the link id + the current contact (survivor) so the handler can
// re-render this partial as the response.
//
// hx-confirm carries the destructive-action confirmation — HTMX's
// built-in modal — so no extra JS is needed (AC #3 "Confirmação
// obrigatória (modal HTMX) antes do POST"). The data-link-reason
// attribute lets a tenant-customised stylesheet style by reason.
var identityPanelTmpl = template.Must(template.New("identity_panel").Funcs(templateFuncs).Parse(`<section id="identity-panel" class="identity-panel" aria-label="Contatos vinculados à identidade">
  <header class="identity-panel__header">
    <h2 class="identity-panel__title">Identidade</h2>
    <span class="identity-panel__id" data-identity-id="{{.Identity.ID}}">{{.Identity.ID}}</span>
  </header>
  {{if .Identity.Links}}
  <ul class="identity-panel__links" role="list">
    {{range .Identity.Links}}
    <li class="identity-link" role="listitem" data-link-reason="{{.Reason}}" data-contact-id="{{.ContactID}}">
      <span class="identity-link__reason">{{linkReasonLabel .Reason}}</span>
      <span class="identity-link__contact">Contato {{.ContactID}}</span>
      <time class="identity-link__time" datetime="{{.CreatedAt.Format "2006-01-02T15:04:05Z07:00"}}">{{formatTime .CreatedAt}}</time>
      {{if ne .ContactID $.ContactID}}
      <form class="identity-link__form"
            hx-post="/contacts/identity/split"
            hx-target="#identity-panel"
            hx-swap="outerHTML"
            hx-confirm="Tem certeza? Esta separação é destrutiva — o merge automático será desfeito.">
        {{$.CSRFInput}}
        <input type="hidden" name="link_id" value="{{.ID}}">
        <input type="hidden" name="survivor_contact_id" value="{{$.ContactID}}">
        <button type="submit" class="identity-link__split">Separar este contato</button>
      </form>
      {{else}}
      <span class="identity-link__self" aria-label="Contato corrente">Contato corrente</span>
      {{end}}
    </li>
    {{end}}
  </ul>
  {{else}}
  <p class="identity-panel__empty">Nenhum contato vinculado a esta identidade.</p>
  {{end}}
</section>
`))

func init() {
	// Cross-register so the layout can {{template "identity_panel" .Panel}}.
	// Errors here are programmer errors — surface them at process start.
	if _, err := contactLayoutTmpl.AddParseTree(identityPanelTmpl.Name(), identityPanelTmpl.Tree); err != nil {
		panic("web/contacts: register identity_panel in layout: " + err.Error())
	}
	// Prime html/template's lazy escaper on every template now, before any
	// concurrent goroutine can race on the first Execute call (the same
	// fix internal/web/inbox carries from SIN-62774).
	for _, t := range []*template.Template{identityPanelTmpl, contactLayoutTmpl} {
		_ = t.Execute(io.Discard, nil)
	}
}
