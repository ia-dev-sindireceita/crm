package users

import "html/template"

// pageData is the view-model for the full page and the list partial.
//
// CSRFMeta and HXHeaders are required because the authed group's CSRF
// middleware rejects POST/PATCH without an X-CSRF-Token header. Every
// mutation on this surface (create, role change, deactivate, reactivate)
// is an HTMX request that bubbles up from inside <body>; hx-headers on
// <body> makes each inherit the token. Without it they 403 (SIN-67232).
type pageData struct {
	TenantName       string
	Users            []userRow
	Count            int
	Flash            flashData
	CSRFMeta         template.HTML
	HXHeaders        template.HTMLAttr
	TenantThemeStyle template.CSS
	CSPNonce         string
}

// userRow decorates a domain user with view-only flags computed in the
// handler (IsSelf, CanDeactivate — the last-active-gerente guard).
type userRow struct {
	ID            string
	Email         string
	Role          string // raw iam.Role string ("tenant_gerente"/"tenant_atendente")
	Active        bool
	IsSelf        bool
	CanDeactivate bool
}

// formData drives the create / edit form partial.
type formData struct {
	Mode        string // "create" | "edit"
	ID          string
	Email       string
	Role        string
	EmailError  string
	GlobalError string
	LockDemote  bool // edit: this user is the last active gerente — disable Atendente
	CanGerente  bool
	CSPNonce    string
}

// flashData is an inline alert shown above the list after a mutation.
type flashData struct {
	Kind    string // "success" | "danger" | "info"
	Message string
}

var funcs = template.FuncMap{
	"roleLabel": func(role string) string {
		switch role {
		case "tenant_gerente":
			return "Gerente"
		case "tenant_atendente":
			return "Atendente"
		default:
			return role
		}
	},
}

// root holds every template; sub-templates are looked up for HTMX partials.
var root = template.Must(template.New("users").Funcs(funcs).Parse(
	pageSrc + flashSrc + listPartialSrc + listBodySrc + formSrc,
))

var (
	pageTmpl        = root.Lookup("users.page")
	listPartialTmpl = root.Lookup("users.listPartial")
	formTmpl        = root.Lookup("users.form")
)

// users.page — full server-rendered page. Head order (SIN-66495 spec):
// tokens → components → users → tenant-theme (branding wins last). HTMX is
// loaded ON the page (the app-shell does not inject it) under the CSP nonce.
const pageSrc = `{{define "users.page"}}<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Usuários — {{.TenantName}}</title>
  {{.CSRFMeta}}
  <link rel="stylesheet" href="/static/css/tokens.css">
  <link rel="stylesheet" href="/static/css/components.css">
  <link rel="stylesheet" href="/static/css/users.css">
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" nonce="{{.CSPNonce}}" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="users-shell" role="main" aria-label="Usuários do tenant">
    <header class="users-header">
      <h1>Usuários — {{.TenantName}}</h1>
      <p class="users-lede">Gerencie quem acessa o CRM deste tenant. Somente administradores podem criar, editar papel e desativar usuários.</p>
    </header>

    <section id="users-list" class="users-list" aria-labelledby="users-list-title">
      <div class="users-list__bar">
        <h2 id="users-list-title">Usuários ({{.Count}})</h2>
        <button type="button" class="btn btn--primary"
                hx-get="/settings/users/new" hx-target="#users-form" hx-swap="innerHTML">Novo usuário</button>
      </div>
      {{template "users.listPartial" .}}
    </section>

    <div id="users-form" aria-live="polite"></div>
  </main>
</body>
</html>
{{end}}`

const flashSrc = `{{define "users.flash"}}{{if .Message}}<div class="alert alert--{{.Kind}}" role="{{if eq .Kind "danger"}}alert{{else}}status{{end}}">{{.Message}}</div>{{end}}{{end}}`

// users.listPartial — the #users-list-inner swap target. Mutations
// (role change, deactivate, reactivate) return this via outerHTML.
const listPartialSrc = `{{define "users.listPartial"}}<div id="users-list-inner">
  {{template "users.flash" .Flash}}
  {{template "users.listBody" .Users}}
</div>{{end}}`

// users.listBody — the table (or empty state). Rendered with the []userRow.
const listBodySrc = `{{define "users.listBody"}}{{if .}}
  <table class="table users-table" aria-label="Lista de usuários do tenant">
    <thead>
      <tr>
        <th scope="col">E-mail</th>
        <th scope="col">Papel</th>
        <th scope="col">Status</th>
        <th scope="col"><span class="sr-only">Ações</span></th>
      </tr>
    </thead>
    <tbody>
    {{- range .}}
      <tr class="users-row{{if not .Active}} users-row--inactive{{end}}">
        <th scope="row" class="users-cell-email">{{.Email}}{{if .IsSelf}} <span class="badge">você</span>{{end}}</th>
        <td>
          {{- if eq .Role "tenant_gerente"}}<span class="badge badge--info">Gerente</span>
          {{- else}}<span class="badge">Atendente</span>{{end}}
        </td>
        <td>
          {{- if .Active}}<span class="badge badge--success">Ativo</span>
          {{- else}}<span class="badge badge--warning">Desativado</span>{{end}}
        </td>
        <td class="users-cell-actions">
          <button type="button" class="btn btn--ghost"
                  hx-get="/settings/users/{{.ID}}/edit" hx-target="#users-form" hx-swap="innerHTML">Editar</button>
          {{- if .Active}}
            {{- if .CanDeactivate}}
          <button type="button" class="btn btn--danger"
                  hx-post="/settings/users/{{.ID}}/deactivate"
                  hx-target="#users-list-inner" hx-swap="outerHTML"
                  hx-confirm="Desativar {{.Email}}? A pessoa perde o acesso, mas o histórico é preservado. Você pode reativar depois.">Desativar</button>
            {{- else}}
          <button type="button" class="btn btn--danger" disabled
                  aria-describedby="lastgerente-hint-{{.ID}}"
                  title="Não é possível desativar o último gerente do tenant">Desativar</button>
          <span id="lastgerente-hint-{{.ID}}" class="users-hint">Último gerente — promova outro admin antes.</span>
            {{- end}}
          {{- else}}
          <button type="button" class="btn"
                  hx-post="/settings/users/{{.ID}}/reactivate"
                  hx-target="#users-list-inner" hx-swap="outerHTML"
                  hx-confirm="Reativar {{.Email}}? A pessoa volta a ter acesso com o papel atual.">Reativar</button>
          {{- end}}
        </td>
      </tr>
    {{- end}}
    </tbody>
  </table>
{{else}}
  <div class="empty-state">
    <h3 class="empty-state__title">Nenhum usuário ainda</h3>
    <p class="empty-state__body">Crie o primeiro usuário deste tenant para dar acesso ao CRM.</p>
  </div>
{{end}}{{end}}`

// users.form — create / edit form. Submits to #users-list-inner (outerHTML);
// on a validation error the handler sets HX-Retarget:#users-form so the
// re-rendered form (carrying the error) lands back in place.
const formSrc = `{{define "users.form"}}
{{- if eq .Mode "edit"}}
<form id="users-form-el" class="users-form card"
      hx-patch="/settings/users/{{.ID}}" hx-target="#users-list-inner" hx-swap="outerHTML"
      hx-disabled-elt="find button[type=submit]">
  <h2 class="card__title">Editar usuário — {{.Email}}</h2>
  {{if .GlobalError}}<div class="alert alert--danger" role="alert">{{.GlobalError}}</div>{{end}}
  <div class="field">
    <label class="field__label" for="edit-email">E-mail</label>
    <input class="field__input" id="edit-email" type="email" value="{{.Email}}" readonly>
    <p class="field__help">O e-mail não pode ser alterado.</p>
  </div>
  <fieldset class="field users-role-field">
    <legend class="field__label">Papel</legend>
    <label class="users-radio"><input type="radio" name="role" value="tenant_atendente"{{if eq .Role "tenant_atendente"}} checked{{end}}{{if .LockDemote}} disabled{{end}}> <span>Atendente</span></label>
    <label class="users-radio"><input type="radio" name="role" value="tenant_gerente"{{if eq .Role "tenant_gerente"}} checked{{end}}> <span>Gerente</span></label>
    {{if .LockDemote}}<p class="field__help">Você é o último gerente ativo. Promova outro usuário a Gerente antes de rebaixar este papel.</p>{{end}}
  </fieldset>
  <div class="users-form__actions">
    <a class="btn btn--ghost" href="/settings/users">Cancelar</a>
    <button type="submit" class="btn btn--primary">Salvar papel</button>
  </div>
</form>
{{- else}}
<form id="users-form-el" class="users-form card"
      hx-post="/settings/users" hx-target="#users-list-inner" hx-swap="outerHTML"
      hx-disabled-elt="find button[type=submit]">
  <h2 class="card__title">Novo usuário</h2>
  {{if .GlobalError}}<div class="alert alert--danger" role="alert">{{.GlobalError}}</div>{{end}}
  <div class="field{{if .EmailError}} field--invalid{{end}}"{{if .EmailError}} data-invalid="true"{{end}}>
    <label class="field__label" for="new-email">E-mail</label>
    <input class="field__input" id="new-email" name="email" type="email" value="{{.Email}}"
           required autocomplete="off" inputmode="email" placeholder="pessoa@dominio.com"
           {{if .EmailError}}aria-invalid="true" aria-describedby="new-email-err"{{end}}>
    {{if .EmailError}}<p class="field__error" id="new-email-err" role="alert">{{.EmailError}}</p>
    {{else}}<p class="field__help">Precisa ser único neste tenant.</p>{{end}}
  </div>
  <fieldset class="field users-role-field">
    <legend class="field__label">Papel</legend>
    <label class="users-radio"><input type="radio" name="role" value="tenant_atendente"{{if ne .Role "tenant_gerente"}} checked{{end}}> <span>Atendente</span> <span class="users-radio__desc">— atende conversas e contatos.</span></label>
    <label class="users-radio"><input type="radio" name="role" value="tenant_gerente"{{if eq .Role "tenant_gerente"}} checked{{end}}> <span>Gerente</span> <span class="users-radio__desc">— administra o tenant (usuários, configurações).</span></label>
  </fieldset>
  <div class="users-form__actions">
    <a class="btn btn--ghost" href="/settings/users">Cancelar</a>
    <button type="submit" class="btn btn--primary">Criar usuário</button>
  </div>
</form>
{{- end}}
{{end}}`
