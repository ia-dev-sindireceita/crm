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
	"convStateLabel":  convStateLabel,
}

// convStateLabel maps an inbox conversation state onto a Portuguese
// label. Unknown states render the raw value so the history list degrades
// safely rather than dropping the row.
func convStateLabel(state string) string {
	switch state {
	case "open":
		return "Aberta"
	case "pending":
		return "Pendente"
	case "resolved":
		return "Resolvida"
	case "closed":
		return "Fechada"
	default:
		return state
	}
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
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
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
    {{with .Detail}}
    <section class="contact-detail" aria-label="Dados do contato">
      <header class="contact-detail__header">
        <h2 class="contact-detail__name" data-contact-name>{{.DisplayName}}</h2>
        <p class="contact-detail__meta">
          <a class="contact-detail__back" href="/contacts" hx-get="/contacts" hx-target="body" hx-push-url="true">← Todos os contatos</a>
        </p>
      </header>
      {{if .Channels}}
      <ul class="contact-detail__channels" role="list" aria-label="Canais">
        {{range .Channels}}<li class="contact-detail__channel" data-channel="{{.}}">{{.}}</li>{{end}}
      </ul>
      {{end}}
      {{if .Identities}}
      <ul class="contact-detail__identities" role="list" aria-label="Identidades de canal">
        {{range .Identities}}<li class="contact-detail__identity"><span class="contact-detail__identity-channel">{{.Channel}}</span><span class="contact-detail__identity-external">{{.ExternalID}}</span></li>{{end}}
      </ul>
      {{end}}
      <section id="contact-edit-panel" class="contact-edit-panel">
        <a class="contact-detail__edit"
           href="/contacts/{{.ID}}/edit"
           hx-get="/contacts/{{.ID}}/edit"
           hx-target="#contact-edit-panel"
           hx-swap="outerHTML">Editar nome</a>
      </section>
      <section class="contact-detail__history" aria-label="Histórico de conversas">
        <h3 class="contact-detail__history-title">Conversas recentes</h3>
        {{if .Conversations}}
        <ul class="contact-detail__conversations" role="list">
          {{range .Conversations}}
          <li class="contact-conversation" data-conversation-id="{{.ID}}" data-channel="{{.Channel}}">
            <span class="contact-conversation__channel">{{.Channel}}</span>
            <span class="contact-conversation__state">{{convStateLabel .State}}</span>
            <time class="contact-conversation__time" datetime="{{.LastMessageAt.Format "2006-01-02T15:04:05Z07:00"}}">{{formatTime .LastMessageAt}}</time>
          </li>
          {{end}}
        </ul>
        {{else}}
        <p class="contact-detail__history-empty">Nenhuma conversa registrada para este contato.</p>
        {{end}}
      </section>
    </section>
    {{end}}
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

// contactsResultsTmpl is the swap unit for the list pane: the table rows
// plus the pager. Search (keyup-debounced hx-get) and the pager links
// both target #contacts-results so one template serves every refresh.
//
// CSP note (SIN-63977): there are NO inline on*= handlers — every
// interaction is an hx-* attribute, which HTMX's own (nonce-allowed)
// script reads. The search input fires on keyup (debounced) and on the
// native "search" event (clearing the box).
var contactsResultsTmpl = template.Must(template.New("contacts_results").Funcs(templateFuncs).Parse(`<div id="contacts-results" class="contacts-results">
  <p class="contacts-results__summary" role="status">
    {{if .Total}}Exibindo {{.From}}–{{.To}} de {{.Total}}{{else}}Nenhum contato encontrado{{end}}{{if .Query}} para “{{.Query}}”{{end}}
  </p>
  <table class="contacts-table">
    <thead>
      <tr><th scope="col">Nome</th><th scope="col">Canais</th><th scope="col">Identidades</th></tr>
    </thead>
    <tbody id="contacts-tbody">
      {{range .Items}}
      <tr class="contacts-row" data-contact-id="{{.ID}}">
        <td class="contacts-row__name"><a href="/contacts/{{.ID}}">{{.DisplayName}}</a></td>
        <td class="contacts-row__channels">{{range .Channels}}<span class="contacts-row__channel" data-channel="{{.}}">{{.}}</span> {{end}}</td>
        <td class="contacts-row__identities">{{range .Identities}}<span class="contacts-row__identity">{{.Channel}}:{{.ExternalID}}</span> {{end}}</td>
      </tr>
      {{else}}
      <tr class="contacts-row contacts-row--empty"><td colspan="3">Nenhum contato corresponde à busca.</td></tr>
      {{end}}
    </tbody>
  </table>
  <nav class="contacts-pager" aria-label="Paginação">
    {{if .HasPrev}}<a class="contacts-pager__prev" href="/contacts?q={{.Query}}&amp;offset={{.PrevOff}}&amp;limit={{.Limit}}" hx-get="/contacts?q={{.Query}}&amp;offset={{.PrevOff}}&amp;limit={{.Limit}}" hx-target="#contacts-results" hx-swap="outerHTML" rel="prev">← Anteriores</a>{{end}}
    {{if .HasNext}}<a class="contacts-pager__next" href="/contacts?q={{.Query}}&amp;offset={{.NextOff}}&amp;limit={{.Limit}}" hx-get="/contacts?q={{.Query}}&amp;offset={{.NextOff}}&amp;limit={{.Limit}}" hx-target="#contacts-results" hx-swap="outerHTML" rel="next">Próximos →</a>{{end}}
  </nav>
</div>
`))

// contactsListTmpl is the full-page list shell. It embeds
// contacts_results for the initial render; subsequent searches/pages swap
// just that fragment.
var contactsListTmpl = template.Must(template.New("contacts.list").Funcs(templateFuncs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Contatos</title>
  {{.CSRFMeta}}
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/contacts.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="contacts-shell" role="main">
    <header class="contacts-shell__header">
      <h1>Contatos</h1>
    </header>
    <form class="contacts-search" role="search" hx-get="/contacts" hx-target="#contacts-results" hx-swap="outerHTML">
      <label class="contacts-search__label" for="contacts-search-input">Buscar contatos</label>
      <input id="contacts-search-input"
             class="contacts-search__input"
             type="search"
             name="q"
             value="{{.Results.Query}}"
             placeholder="Nome, telefone ou e-mail…"
             autocomplete="off"
             hx-get="/contacts"
             hx-trigger="keyup changed delay:300ms, search"
             hx-target="#contacts-results"
             hx-swap="outerHTML">
    </form>
    {{template "contacts_results" .Results}}
  </main>
</body>
</html>
`))

// contactEditPanelTmpl is the edit form fragment (swap unit for
// #contact-edit-panel). It is reused as the 422 re-render with an inline
// error. CSP-safe: hx-post drives the submit, no inline handlers.
var contactEditPanelTmpl = template.Must(template.New("contact_edit_panel").Funcs(templateFuncs).Parse(`<section id="contact-edit-panel" class="contact-edit-panel">
  <form class="contact-edit-form"
        hx-post="/contacts/{{.ContactID}}/edit"
        hx-target="#contact-edit-panel"
        hx-swap="outerHTML">
    {{.CSRFInput}}
    <label class="contact-edit-form__label" for="contact-edit-name">Nome de exibição</label>
    <input id="contact-edit-name" class="contact-edit-form__input" type="text" name="display_name" value="{{.DisplayName}}" required>
    {{if .Error}}<p class="contact-edit-form__error" role="alert">{{.Error}}</p>{{end}}
    <div class="contact-edit-form__actions">
      <button type="submit" class="contact-edit-form__save">Salvar</button>
      <a class="contact-edit-form__cancel" href="/contacts/{{.ContactID}}" hx-get="/contacts/{{.ContactID}}/edit" hx-target="#contact-edit-panel" hx-swap="outerHTML">Cancelar</a>
    </div>
  </form>
</section>
`))

// contactSavedPanelTmpl replaces the form after a successful HTMX save:
// the new name + an affordance to edit again. It keeps the
// #contact-edit-panel id so a subsequent edit click swaps cleanly.
var contactSavedPanelTmpl = template.Must(template.New("contact_saved_panel").Funcs(templateFuncs).Parse(`<section id="contact-edit-panel" class="contact-edit-panel">
  <p class="contact-edit-panel__saved" role="status">Nome atualizado.</p>
  <p class="contact-edit-panel__name" data-contact-name>{{.Contact.DisplayName}}</p>
  <a class="contact-detail__edit"
     href="/contacts/{{.Contact.ID}}/edit"
     hx-get="/contacts/{{.Contact.ID}}/edit"
     hx-target="#contact-edit-panel"
     hx-swap="outerHTML">Editar nome</a>
</section>
`))

// contactEditPageTmpl is the full-page edit shell for a direct navigation
// to /contacts/{id}/edit (progressive enhancement: the form works without
// HTMX). It embeds contact_edit_panel.
var contactEditPageTmpl = template.Must(template.New("contact.edit").Funcs(templateFuncs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Editar contato</title>
  {{.CSRFMeta}}
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/contacts.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="contact-shell" role="main">
    <header class="contact-shell__header">
      <h1>Editar contato</h1>
      <p class="contact-shell__hint"><a href="/contacts/{{.Form.ContactID}}">← Voltar ao contato</a></p>
    </header>
    {{template "contact_edit_panel" .Form}}
  </main>
</body>
</html>
`))

func init() {
	// Cross-register so the layout can {{template "identity_panel" .Panel}}.
	// Errors here are programmer errors — surface them at process start.
	if _, err := contactLayoutTmpl.AddParseTree(identityPanelTmpl.Name(), identityPanelTmpl.Tree); err != nil {
		panic("web/contacts: register identity_panel in layout: " + err.Error())
	}
	// The list shell embeds contacts_results; the edit page embeds
	// contact_edit_panel. Cross-register both so {{template}} resolves.
	if _, err := contactsListTmpl.AddParseTree(contactsResultsTmpl.Name(), contactsResultsTmpl.Tree); err != nil {
		panic("web/contacts: register contacts_results in list: " + err.Error())
	}
	if _, err := contactEditPageTmpl.AddParseTree(contactEditPanelTmpl.Name(), contactEditPanelTmpl.Tree); err != nil {
		panic("web/contacts: register contact_edit_panel in edit page: " + err.Error())
	}
	// Prime html/template's lazy escaper on every template now, before any
	// concurrent goroutine can race on the first Execute call (the same
	// fix internal/web/inbox carries from SIN-62774).
	for _, t := range []*template.Template{
		identityPanelTmpl, contactLayoutTmpl,
		contactsResultsTmpl, contactsListTmpl,
		contactEditPanelTmpl, contactSavedPanelTmpl, contactEditPageTmpl,
	} {
		_ = t.Execute(io.Discard, nil)
	}
}
