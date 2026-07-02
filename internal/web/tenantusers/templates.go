package tenantusers

import (
	"html/template"
	"io"

	"github.com/pericles-luz/crm/internal/web/shell"
)

// funcs is intentionally empty — every label is pre-computed in Go (view.go)
// so the templates stay logic-free and CSP-clean (no inline handlers, no
// client branching).
var funcs = template.FuncMap{}

// partialDefs holds every shared sub-template: the user row, the table body,
// the create/edit modal form, the one-time credential card and the toast.
// Each callable tree below parses this block so {{template …}} resolves in
// the page, the modal fragment and the OOB responses alike.
//
// Strict-CSP note: no inline on*= / hx-on:* anywhere; all interactivity is
// hx-get / hx-post attributes and OOB swaps (memory
// reference_crm_csp_inline_handler_silent_break).
const partialDefs = `
{{define "tenantusers.row"}}
<tr id="user-row-{{.ID}}" class="users-row{{if not .Active}} users-row--inactive{{end}}">
  <td class="users-row__email">{{.Email}}</td>
  <td class="users-row__role">{{.RoleLabel}}</td>
  <td>
    {{if .Active}}<span class="badge badge--success">Ativo</span>{{else}}<span class="badge">Inativo</span>{{end}}
    {{if .MustChangePassword}}<span class="badge badge--info users-row__pending" title="Senha temporária — o usuário ainda não trocou a senha no primeiro login.">Senha temporária</span>{{end}}
  </td>
  <td class="users-row__actions">
    <button type="button" class="btn btn--ghost" hx-get="/settings/users/{{.ID}}/edit" hx-target="#tenantusers-modal" hx-swap="innerHTML">Editar papel</button>
    {{if .Active}}
    <button type="button" class="btn btn--ghost" hx-post="/settings/users/{{.ID}}/deactivate" hx-target="#tenantusers-list" hx-swap="innerHTML" hx-confirm="Desativar {{.Email}}? O usuário deixa de conseguir entrar; o histórico é preservado.">Desativar</button>
    {{end}}
  </td>
</tr>
{{end}}

{{define "tenantusers.listBody"}}
{{- if .Rows}}
<table class="users-list table">
  <thead><tr><th>E-mail</th><th>Papel</th><th>Status</th><th>Ações</th></tr></thead>
  <tbody>
  {{- range .Rows}}{{template "tenantusers.row" .}}{{- end}}
  </tbody>
</table>
{{- else}}
<div class="empty-state" data-testid="tenantusers-empty">
  <h2>Nenhum usuário cadastrado</h2>
  <p>Crie o primeiro usuário para dar acesso à equipe.</p>
  <a class="btn btn--primary" href="/settings/users/new" hx-get="/settings/users/new" hx-target="#tenantusers-modal" hx-swap="innerHTML">+ Novo usuário</a>
</div>
{{- end}}
{{end}}

{{define "tenantusers.modalForm"}}
<div class="modal" role="dialog" aria-modal="true" aria-labelledby="tenantusers-modal-title">
  <div class="modal__dialog users-modal">
    <h2 class="modal__title" id="tenantusers-modal-title">{{if .IsNew}}Novo usuário{{else}}Editar papel{{end}}</h2>
    <form hx-post="{{.Action}}" hx-target="#tenantusers-modal" hx-swap="innerHTML" class="users-form">
      {{- if and .ErrorMessage (eq .FieldError "")}}
      <div class="alert alert--danger" role="alert">{{.ErrorMessage}}</div>
      {{- end}}
      <div class="field">
        <label for="user-email">E-mail</label>
        <input class="field__input" id="user-email" name="email" type="email" value="{{.Email}}" {{if .IsNew}}required autofocus{{else}}readonly{{end}} maxlength="254">
        {{if .IsNew}}<p class="field__help">O usuário entra com este e-mail. Uma senha temporária será gerada e exibida uma única vez.</p>{{end}}
        {{if eq .FieldError "email"}}<p class="field__error" role="alert">{{.ErrorMessage}}</p>{{end}}
      </div>
      <div class="field">
        <label for="user-role">Papel</label>
        <select class="field__select" id="user-role" name="role" required>
          {{- range .Roles}}
          <option value="{{.Key}}"{{if eq .Key $.RoleKey}} selected{{end}}>{{.Label}}</option>
          {{- end}}
        </select>
        <p class="field__help">Gerente administra usuários e configurações do tenant; Atendente atende conversas.</p>
        {{if eq .FieldError "role"}}<p class="field__error" role="alert">{{.ErrorMessage}}</p>{{end}}
      </div>
      <div class="modal__actions">
        <button type="submit" class="btn btn--primary">{{if .IsNew}}Criar usuário{{else}}Salvar papel{{end}}</button>
        <button type="button" class="btn btn--ghost" hx-get="/settings/users/cancel" hx-target="#tenantusers-modal" hx-swap="innerHTML">Cancelar</button>
      </div>
    </form>
  </div>
</div>
{{end}}

{{define "tenantusers.credential"}}
<div class="modal" role="dialog" aria-modal="true" aria-labelledby="tenantusers-cred-title">
  <div class="modal__dialog users-credential" data-testid="tenantusers-credential">
    <h2 class="modal__title" id="tenantusers-cred-title">Usuário criado</h2>
    <div class="alert alert--warning" role="alert">
      Esta senha temporária é exibida <strong>uma única vez</strong>. Copie e entregue ao usuário por um canal seguro. Ela não pode ser recuperada depois — se for perdida, crie o usuário novamente ou redefina a senha.
    </div>
    <dl class="users-credential__grid">
      <dt>E-mail</dt><dd><code class="users-credential__value">{{.Email}}</code></dd>
      <dt>Senha temporária</dt><dd><code class="users-credential__value" data-testid="tenantusers-temp-password">{{.TempPassword}}</code></dd>
    </dl>
    <p class="field__help">O usuário deverá trocar a senha no primeiro acesso.</p>
    <div class="modal__actions">
      <button type="button" class="btn btn--primary" hx-get="/settings/users/cancel" hx-target="#tenantusers-modal" hx-swap="innerHTML">Entendi, fechar</button>
    </div>
  </div>
</div>
{{end}}

{{define "tenantusers.toast"}}
<div class="alert alert--success users-toast__msg" role="status">{{.Message}}</div>
{{end}}
`

// mustTmpl builds a standalone fragment template: the shared partial defs
// plus the supplied root body. The root is parsed first so it owns rootName;
// the defs are then added to the same tree. The tree is primed against
// io.Discard to warm html/template's lazy escaper before any concurrent
// request (the AddParseTree race the dashboard/inbox surfaces hit — memory
// reference_crm_html_template_race).
func mustTmpl(rootName, rootBody string) *template.Template {
	t := template.Must(template.New(rootName).Funcs(funcs).Parse(rootBody))
	template.Must(t.Parse(partialDefs))
	_ = t.Execute(io.Discard, nil)
	return t
}

// modalTmpl renders the create/edit modal form standalone (GET /new,
// GET /{id}/edit, and the validation-error re-render).
var modalTmpl = mustTmpl("tenantusers.modal", `{{template "tenantusers.modalForm" .}}`)

// refreshTmpl is the OOB response after a role change / deactivate: it
// re-renders the whole table into the primary #tenantusers-list target plus
// an OOB toast. (The list is the primary target because the row buttons
// hx-target #tenantusers-list.)
var refreshTmpl = mustTmpl("tenantusers.refresh", `{{template "tenantusers.listBody" .}}<div id="tenantusers-toast" class="users-toast" aria-live="polite" hx-swap-oob="innerHTML">{{template "tenantusers.toast" .Toast}}</div>`)

// createTmpl is the OOB response after a successful create: the one-time
// credential card (primary target #tenantusers-modal) + the OOB list refresh
// + an OOB toast.
var createTmpl = mustTmpl("tenantusers.create", `{{template "tenantusers.credential" .Credential}}<div id="tenantusers-list" class="card users-card" hx-swap-oob="true">{{template "tenantusers.listBody" .}}</div><div id="tenantusers-toast" class="users-toast" aria-live="polite" hx-swap-oob="innerHTML">{{template "tenantusers.toast" .Toast}}</div>`)

// pageTmpl is the full users page on the shared SidebarNav app-shell. The
// page stylesheet + htmx are injected via "head_extra" (the shell head links
// tokens.css → components.css; users.css comes after, and the shell does NOT
// inject htmx so the surface loads its own — memory
// reference_crm_shell_surface_needs_own_htmx).
var pageTmpl = func() *template.Template {
	t := shell.MustParse(funcs, nil)
	template.Must(t.Parse(partialDefs))
	template.Must(t.Parse(`
{{define "title"}}Usuários{{end}}
{{define "head_extra"}}
  <link rel="stylesheet" href="/static/css/users.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" nonce="{{shellCSPNonce .}}" defer></script>
{{end}}
{{define "content"}}
  <div class="users-page" data-testid="tenantusers">
    <div class="users-page__header">
      <h1 class="users-page__title">Usuários</h1>
      <a class="btn btn--primary" href="/settings/users/new" hx-get="/settings/users/new" hx-target="#tenantusers-modal" hx-swap="innerHTML">+ Novo usuário</a>
    </div>
    <p class="users-page__lede">Gerencie quem tem acesso ao tenant: crie usuários, altere papéis e desative acessos. Desativar preserva o histórico do usuário.</p>
    <div id="tenantusers-toast" class="users-toast" aria-live="polite"></div>
    <div id="tenantusers-list" class="card users-card">{{template "tenantusers.listBody" .}}</div>
    <div id="tenantusers-modal"></div>
  </div>
{{end}}
`))
	_ = t.Execute(io.Discard, nil)
	return t.Lookup("layout")
}()
