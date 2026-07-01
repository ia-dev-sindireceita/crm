package channels

import (
	"html/template"
	"io"

	"github.com/pericles-luz/crm/internal/web/shell"
)

// listRefresh drives the OOB response after a create / edit: the whole
// registry list is re-rendered (a new row appears or an edited row's
// summary changes) and a success toast is swapped in, while the modal
// closes because the primary #channels-modal target receives no
// non-OOB content.
type listRefresh struct {
	Rows  []channelRow
	Toast toastData
}

// rowRefresh drives the OOB response after an activate/deactivate toggle:
// the single row is swapped in place (primary target) plus a toast (OOB).
type rowRefresh struct {
	Row   channelRow
	Toast toastData
}

// funcs is intentionally empty — every label/summary is pre-computed in
// Go (view.go) so the templates stay logic-free and CSP-clean (no inline
// handlers, no client branching).
var funcs = template.FuncMap{}

// partialDefs holds every shared sub-template: the registry row, the
// list body, the access-roster primitive, the modal form and the toast.
// Each callable tree below parses this block so {{template …}} resolves
// in the page, the modal fragment and the OOB responses alike.
const partialDefs = `
{{define "channels.row"}}
<tr id="channel-row-{{.ID}}" class="channels-row{{if not .Active}} channels-row--inactive{{end}}">
  <td class="channels-row__name">{{.Name}}</td>
  <td class="channels-row__type">{{.TypeLabel}}</td>
  <td class="channels-row__identity">{{.MaskedIdentity}}</td>
  <td><span class="badge{{if not .AccessAll}} badge--info{{end}} channels-access-summary">{{.AccessSummary}}</span></td>
  <td>{{if .Active}}<span class="badge badge--success">Ativo</span>{{else}}<span class="badge">Inativo</span>{{end}}</td>
  <td class="channels-row__actions">
    <button type="button" class="btn btn--ghost" hx-get="/settings/channels/{{.ID}}/edit" hx-target="#channels-modal" hx-swap="innerHTML">Editar</button>
    <button type="button" class="btn btn--ghost" hx-post="/settings/channels/{{.ID}}/active" hx-target="#channel-row-{{.ID}}" hx-swap="outerHTML">{{if .Active}}Desativar{{else}}Ativar{{end}}</button>
  </td>
</tr>
{{end}}

{{define "channels.listBody"}}
{{- if .Rows}}
<table class="channels-list table">
  <thead><tr><th>Canal</th><th>Tipo</th><th>Identidade</th><th>Acesso</th><th>Status</th><th>Ações</th></tr></thead>
  <tbody>
  {{- range .Rows}}{{template "channels.row" .}}{{- end}}
  </tbody>
</table>
{{- else}}
<div class="empty-state" data-testid="channels-empty">
  <h2>Nenhum canal configurado</h2>
  <p>Configure o primeiro canal para começar a receber mensagens.</p>
  <a class="btn btn--primary" href="/settings/channels/new" hx-get="/settings/channels/new" hx-target="#channels-modal" hx-swap="innerHTML">+ Novo canal</a>
</div>
{{- end}}
{{end}}

{{define "channels.roster"}}
<fieldset id="access-roster" class="channels-roster">
  <legend>Quem atende este canal</legend>
  <p class="field__help">Todos os atendentes ativos começam marcados. Desmarque quem não deve ver estas conversas.</p>
  {{- range .Entries}}
  <label class="channels-roster__row">
    <input type="checkbox" name="user_ids" value="{{.ID}}"{{if .Checked}} checked{{end}}>
    <span class="channels-roster__name">{{.DisplayName}}</span>
    <span class="channels-roster__role">{{.RoleLabel}}</span>
  </label>
  {{- else}}
  <p class="channels-roster__empty">Nenhum atendente ativo neste tenant.</p>
  {{- end}}
  <p class="channels-roster__count" id="access-count" aria-live="polite">{{.Checked}} de {{.Total}} com acesso</p>
</fieldset>
{{end}}

{{define "channels.modalForm"}}
<div class="modal" role="dialog" aria-modal="true" aria-labelledby="channels-modal-title">
  <div class="modal__dialog channels-modal">
    <h2 class="modal__title" id="channels-modal-title">{{if .IsNew}}Novo canal{{else}}Editar canal{{end}}</h2>
    <form hx-post="{{.Action}}" hx-target="#channels-modal" hx-swap="innerHTML" class="channels-form">
      {{- if and .ErrorMessage (eq .FieldError "")}}
      <div class="alert alert--danger" role="alert">{{.ErrorMessage}}</div>
      {{- end}}
      <div class="field">
        <label for="channel-name">Nome de exibição</label>
        <input class="field__input" id="channel-name" name="name" value="{{.Name}}" required maxlength="120" autofocus>
        <p class="field__help">Como este canal aparece para a equipe.</p>
        {{if eq .FieldError "name"}}<p class="field__error" role="alert">{{.ErrorMessage}}</p>{{end}}
      </div>
      <div class="field">
        <label for="channel-type">Tipo</label>
        <select class="field__select" id="channel-type" name="channel_key"{{if not .IsNew}} disabled{{end}}>
          {{- range .Types}}
          <option value="{{.Key}}"{{if eq .Key $.ChannelKey}} selected{{end}}>{{.Label}}</option>
          {{- end}}
        </select>
        {{if not .IsNew}}<input type="hidden" name="channel_key" value="{{.ChannelKey}}">{{end}}
        {{if eq .FieldError "type"}}<p class="field__error" role="alert">{{.ErrorMessage}}</p>{{end}}
      </div>
      <div class="field">
        <label for="channel-identity">Identidade</label>
        <input class="field__input" id="channel-identity" name="identity" value="{{.Identity}}"{{if not .IsNew}} readonly{{end}} maxlength="120" placeholder="ex.: +55 11 90000-0000">
        <p class="field__help">Número, usuário ou endereço do canal.</p>
        {{if eq .FieldError "identity"}}<p class="field__error" role="alert">{{.ErrorMessage}}</p>{{end}}
      </div>
      <div class="field channels-form__roster">
        <span class="field__legend">Acesso da equipe</span>
        {{template "channels.roster" .Roster}}
      </div>
      <div class="modal__actions">
        <button type="submit" class="btn btn--primary">Salvar canal</button>
        <button type="button" class="btn btn--ghost" hx-get="/settings/channels/cancel" hx-target="#channels-modal" hx-swap="innerHTML">Cancelar</button>
      </div>
    </form>
  </div>
</div>
{{end}}

{{define "channels.toast"}}
<div class="alert alert--success channels-toast__msg" role="status">{{.Message}}</div>
{{end}}
`

// mustTmpl builds a standalone fragment template: the shared partial defs
// plus the supplied root body. The root is parsed first so it owns
// rootName; the defs are then added to the same tree. The tree is primed
// against io.Discard to warm html/template's lazy escaper before any
// concurrent request (the AddParseTree race the dashboard/inbox surfaces
// hit — memory reference_crm_html_template_race).
func mustTmpl(rootName, rootBody string) *template.Template {
	t := template.Must(template.New(rootName).Funcs(funcs).Parse(rootBody))
	template.Must(t.Parse(partialDefs))
	_ = t.Execute(io.Discard, nil)
	return t
}

// modalTmpl renders the create/edit modal form standalone (GET /new,
// GET /{id}/edit, and the validation-error re-render).
var modalTmpl = mustTmpl("channels.modal", `{{template "channels.modalForm" .}}`)

// refreshTmpl is the OOB response after a create/edit: it re-renders the
// whole list (OOB) + a toast (OOB); the empty primary content closes the
// modal.
var refreshTmpl = mustTmpl("channels.refresh", `<div id="channels-list" class="card channels-card" hx-swap-oob="true">{{template "channels.listBody" .}}</div><div id="channels-toast" class="channels-toast" aria-live="polite" hx-swap-oob="innerHTML">{{template "channels.toast" .Toast}}</div>`)

// rowTmpl is the OOB response after a toggle: the single row (primary
// target) + a toast (OOB).
var rowTmpl = mustTmpl("channels.rowResp", `{{template "channels.row" .Row}}<div id="channels-toast" class="channels-toast" aria-live="polite" hx-swap-oob="innerHTML">{{template "channels.toast" .Toast}}</div>`)

// pageTmpl is the full registry page on the shared SidebarNav app-shell.
// The page stylesheet + htmx are injected via "head_extra" (the shell
// head links tokens.css → components.css; channels.css comes after, and
// the shell does NOT inject htmx so the surface loads its own — memory
// reference_crm_shell_surface_needs_own_htmx).
var pageTmpl = func() *template.Template {
	t := shell.MustParse(funcs, nil)
	template.Must(t.Parse(partialDefs))
	template.Must(t.Parse(`
{{define "title"}}Canais{{end}}
{{define "head_extra"}}
  <link rel="stylesheet" href="/static/css/channels.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" nonce="{{shellCSPNonce .}}" defer></script>
{{end}}
{{define "content"}}
  <div class="channels-page" data-testid="channels">
    <div class="channels-page__header">
      <h1 class="channels-page__title">Canais</h1>
      <a class="btn btn--primary" href="/settings/channels/new" hx-get="/settings/channels/new" hx-target="#channels-modal" hx-swap="innerHTML">+ Novo canal</a>
    </div>
    <div id="channels-toast" class="channels-toast" aria-live="polite"></div>
    <div id="channels-list" class="card channels-card">{{template "channels.listBody" .}}</div>
    <div id="channels-modal"></div>
  </div>
{{end}}
`))
	_ = t.Execute(io.Discard, nil)
	return t.Lookup("layout")
}()
