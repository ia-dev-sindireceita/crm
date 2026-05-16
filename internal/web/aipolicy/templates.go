package aipolicy

import (
	"html/template"

	"github.com/pericles-luz/crm/internal/aipolicy"
)

// pageData is the full-page view-model. Embeds the partials' view-
// models so the same struct seeds both the initial render and the
// post-mutation HTMX refresh.
type pageData struct {
	TenantName   string
	GeneratedAt  string
	Rows         []rowData
	Preview      previewData
	FormDefaults formDefaults
}

// listPartialData is the view-model returned to HTMX after a
// create/update/delete swap.
type listPartialData struct {
	Rows    []rowData
	Preview previewData
	Now     string
}

// rowData is one table row. Times are pre-formatted by the handler so
// the template stays free of time package imports.
type rowData struct {
	ScopeType string
	ScopeID   string
	Model     string
	Tone      string
	Language  string
	AIEnabled bool
	Anonymize bool
	OptIn     bool
	UpdatedAt string
}

// formData seeds the new and edit form. IsNew flips the form action
// + method between POST (create) and PATCH (update). The enum lists
// drive the <select> options so a future allowlist change ripples
// without a template edit.
type formData struct {
	Action           string
	Method           string
	ScopeType        string
	ScopeID          string
	Model            string
	Tone             string
	Language         string
	AIEnabled        bool
	Anonymize        bool
	OptIn            bool
	IsNew            bool
	AllowedModels    []string
	AllowedTones     []string
	AllowedLanguages []string
}

// previewData is the cascade-preview card view-model. Source is the
// raw aipolicy.ResolveSource string so the template can render
// human-readable labels via a switch.
type previewData struct {
	Policy    aipolicy.Policy
	Source    string
	ChannelID string
	TeamID    string
}

// formDefaults pre-seeds the new-policy form on the full page so the
// admin sees the allowed enums without having to click "novo".
type formDefaults struct {
	AllowedModels    []string
	AllowedTones     []string
	AllowedLanguages []string
	Anonymize        bool
}

// funcs is the template helper set shared by every template.
var funcs = template.FuncMap{
	"sourceLabel": func(src string) string {
		switch src {
		case string(aipolicy.SourceChannel):
			return "Canal (override mais específico)"
		case string(aipolicy.SourceTeam):
			return "Equipe (override)"
		case string(aipolicy.SourceTenant):
			return "Tenant (configuração padrão)"
		case string(aipolicy.SourceDefault):
			return "Padrão do sistema (nenhuma policy configurada)"
		default:
			return src
		}
	},
	"scopeLabel": func(scope string) string {
		switch scope {
		case string(aipolicy.ScopeChannel):
			return "Canal"
		case string(aipolicy.ScopeTeam):
			return "Equipe"
		case string(aipolicy.ScopeTenant):
			return "Tenant"
		default:
			return scope
		}
	},
	"yesno": func(b bool) string {
		if b {
			return "sim"
		}
		return "não"
	},
}

// previewPartial is the cascade preview card markup. Defined as a
// {{define}} block so it can be reused by the page, the post-mutation
// partial, and the standalone preview response. Parsed first so the
// page/list templates can reference it via {{template …}}.
const previewPartialSrc = `
{{define "aipolicy.previewCard"}}
<article class="aipolicy-preview-card aipolicy-preview-card--{{.Source}}">
  <header>
    <h4>Origem: {{sourceLabel .Source}}</h4>
    {{- if .ChannelID}}<p><small>Canal: <code>{{.ChannelID}}</code></small></p>{{end}}
    {{- if .TeamID}}<p><small>Equipe: <code>{{.TeamID}}</code></small></p>{{end}}
  </header>
  <dl>
    <dt>IA habilitada</dt><dd>
      {{- if .Policy.AIEnabled}}
        <strong>sim</strong>
      {{- else}}
        <strong>não — IA desabilitada neste escopo</strong>
      {{- end}}
    </dd>
    <dt>Modelo</dt><dd><code>{{.Policy.Model}}</code></dd>
    <dt>Tom</dt><dd>{{.Policy.Tone}}</dd>
    <dt>Idioma</dt><dd>{{.Policy.Language}}</dd>
    <dt>Prompt</dt><dd><code>{{.Policy.PromptVersion}}</code></dd>
    <dt>Anonimização</dt><dd>{{yesno .Policy.Anonymize}}</dd>
    <dt>Opt-in</dt><dd>{{yesno .Policy.OptIn}}</dd>
  </dl>
</article>
{{end}}
`

// listPartialSrc is the table + inline preview returned to HTMX after
// any mutation. Mounted as a {{define}} block so both the page and
// the post-mutation response can render it.
const listPartialSrc = `
{{define "aipolicy.listPartial"}}
{{- if .Rows}}
<table class="aipolicy-table">
  <thead>
    <tr>
      <th scope="col">Escopo</th>
      <th scope="col">ID</th>
      <th scope="col">Modelo</th>
      <th scope="col">Tom</th>
      <th scope="col">Idioma</th>
      <th scope="col">IA habilitada?</th>
      <th scope="col">Anonimiza?</th>
      <th scope="col">Opt-in?</th>
      <th scope="col">Atualizado em</th>
      <th scope="col">Ações</th>
    </tr>
  </thead>
  <tbody>
  {{- range .Rows}}
    <tr class="aipolicy-row aipolicy-row--{{.ScopeType}}{{if not .AIEnabled}} aipolicy-row--disabled{{end}}">
      <td>{{scopeLabel .ScopeType}}</td>
      <td><code>{{.ScopeID}}</code></td>
      <td><code>{{.Model}}</code></td>
      <td>{{.Tone}}</td>
      <td>{{.Language}}</td>
      <td>{{yesno .AIEnabled}}</td>
      <td>{{yesno .Anonymize}}</td>
      <td>{{yesno .OptIn}}</td>
      <td><time datetime="{{.UpdatedAt}}">{{.UpdatedAt}}</time></td>
      <td>
        <button type="button"
                hx-get="/settings/ai-policy/{{.ScopeType}}/{{.ScopeID}}/edit"
                hx-target="#aipolicy-form"
                hx-swap="innerHTML">Editar</button>
        <button type="button"
                hx-delete="/settings/ai-policy/{{.ScopeType}}/{{.ScopeID}}"
                hx-target="#aipolicy-list"
                hx-swap="innerHTML"
                hx-confirm="Remover policy de {{scopeLabel .ScopeType}} ({{.ScopeID}})?">Remover</button>
      </td>
    </tr>
  {{- end}}
  </tbody>
</table>
{{- else}}
<p class="aipolicy-empty">
  Nenhuma policy configurada — a resolução cai para o padrão do sistema
  (<code>ai_enabled = false</code>, modelo <code>openrouter/auto</code>).
</p>
{{- end}}

<aside class="aipolicy-preview-inline" aria-label="Resolução atual no escopo padrão">
  <h3>Resolução padrão (sem escopo de canal/equipe)</h3>
  {{template "aipolicy.previewCard" .Preview}}
</aside>
{{end}}
`

// pageTmpl is the full page render. HTMX is loaded for the preview
// widget and the create/edit swap. All inline style/script is
// avoided so the page composes with the strict CSP envelope from
// SIN-62237 (no nonce wiring needed in templates that ship zero
// inline elements).
var pageTmpl = mustParse("aipolicy.page", `<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Configuração de IA — {{.TenantName}}</title>
  <link rel="stylesheet" href="/static/css/aipolicy.css">
  <script src="/static/vendor/htmx.min.js" defer></script>
</head>
<body>
  <main class="aipolicy-shell" role="main" aria-label="Configuração de IA">
    <header class="aipolicy-header">
      <h1>Configuração de IA — {{.TenantName}}</h1>
      <p class="aipolicy-lede">
        Esta tela controla quando o CRM aciona a IA, qual modelo é usado,
        com qual tom e em qual idioma. As mudanças seguem a cascata
        <strong>canal &gt; equipe &gt; tenant</strong>: a configuração
        mais específica vence. A flag <code>ai_enabled</code> é a chave
        mestra para desligar o uso de IA em um escopo.
      </p>
      <p class="aipolicy-meta"><small>Gerado em {{.GeneratedAt}}.</small></p>
    </header>

    <section id="aipolicy-list" class="aipolicy-list" aria-labelledby="list-title">
      <h2 id="list-title">Policies configuradas</h2>
      {{template "aipolicy.listPartial" .}}
    </section>

    <section class="aipolicy-new" aria-labelledby="new-title">
      <h2 id="new-title">Criar nova policy</h2>
      <button type="button"
              hx-get="/settings/ai-policy/new"
              hx-target="#aipolicy-form"
              hx-swap="innerHTML">Abrir formulário</button>
      <div id="aipolicy-form" aria-live="polite"></div>
    </section>

    <section class="aipolicy-preview" aria-labelledby="preview-title">
      <h2 id="preview-title">Pré-visualizar resolução em cascata</h2>
      <p>Informe um canal e/ou equipe para ver qual policy será aplicada.</p>
      <form hx-get="/settings/ai-policy/preview"
            hx-target="#aipolicy-preview-card"
            hx-swap="innerHTML"
            hx-trigger="change">
        <label>
          Canal:
          <input type="text" name="channel_id" placeholder="ex. whatsapp">
        </label>
        <label>
          Equipe:
          <input type="text" name="team_id" placeholder="uuid da equipe">
        </label>
      </form>
      <div id="aipolicy-preview-card" aria-live="polite">
        {{template "aipolicy.previewCard" .Preview}}
      </div>
    </section>
  </main>
</body>
</html>
`)

// listPartialTmpl is the table + preview card returned to HTMX after
// any mutation. Executes the listPartial define directly with the
// caller's data via template.Lookup so HTMX clients get just the
// partial fragment.
var listPartialTmpl = mustParse("aipolicy.listPartial.invoke", `{{template "aipolicy.listPartial" .}}`)

// previewTmpl is the standalone preview card response for the HTMX
// preview-form swap.
var previewTmpl = mustParse("aipolicy.preview.invoke", `{{template "aipolicy.previewCard" .}}`)

// formTmpl is the new/edit form. HTMX submits with the configured
// method; the response replaces #aipolicy-list (the table) so the
// new/updated row appears without a full reload.
var formTmpl = mustParse("aipolicy.form", `
<form id="aipolicy-form-element"
      hx-{{.Method}}="{{.Action}}"
      hx-target="#aipolicy-list"
      hx-swap="innerHTML">
  <fieldset>
    <legend>{{if .IsNew}}Nova policy{{else}}Editar policy ({{scopeLabel .ScopeType}} · {{.ScopeID}}){{end}}</legend>

    <label>
      Escopo:
      <select name="scope_type" {{if not .IsNew}}disabled{{end}} required>
        <option value="">— selecione —</option>
        <option value="tenant"  {{if eq .ScopeType "tenant"}}selected{{end}}>Tenant</option>
        <option value="team"    {{if eq .ScopeType "team"}}selected{{end}}>Equipe</option>
        <option value="channel" {{if eq .ScopeType "channel"}}selected{{end}}>Canal</option>
      </select>
    </label>

    <label>
      ID do escopo:
      <input type="text" name="scope_id" value="{{.ScopeID}}"
             {{if not .IsNew}}readonly{{end}} required maxlength="128"
             placeholder="ex. whatsapp ou uuid da equipe">
    </label>

    <label>
      Modelo:
      <select name="model" required>
        <option value="">— selecione —</option>
        {{- range .AllowedModels}}
        <option value="{{.}}" {{if eq . $.Model}}selected{{end}}>{{.}}</option>
        {{- end}}
      </select>
    </label>

    <label>
      Tom:
      <select name="tone" required>
        {{- range .AllowedTones}}
        <option value="{{.}}" {{if eq . $.Tone}}selected{{end}}>{{.}}</option>
        {{- end}}
      </select>
    </label>

    <label>
      Idioma:
      <select name="language" required>
        {{- range .AllowedLanguages}}
        <option value="{{.}}" {{if eq . $.Language}}selected{{end}}>{{.}}</option>
        {{- end}}
      </select>
    </label>

    <label class="aipolicy-toggle">
      <input type="checkbox" name="ai_enabled" value="on" {{if .AIEnabled}}checked{{end}}>
      Habilitar IA neste escopo (desmarque para desligar a IA)
    </label>

    <label class="aipolicy-toggle">
      <input type="checkbox" name="anonymize" value="on" {{if .Anonymize}}checked{{end}}>
      Anonimizar dados antes de enviar (recomendado)
    </label>

    <label class="aipolicy-toggle">
      <input type="checkbox" name="opt_in" value="on" {{if .OptIn}}checked{{end}}>
      Tenant deu opt-in explícito (LGPD)
    </label>

    <button type="submit">{{if .IsNew}}Criar policy{{else}}Atualizar policy{{end}}</button>
  </fieldset>
</form>
`)

// errorPartialTmpl is the 422 response rendered when form validation
// fails. The form re-render path uses the parent form (HTMX swaps
// only this fragment back) so the user keeps their inputs while the
// problem is surfaced.
var errorPartialTmpl = mustParse("aipolicy.error", `
<aside class="aipolicy-form-error" role="alert" data-field="{{.Field}}">
  <strong>Erro no campo {{.Field}}:</strong> {{.Message}}
</aside>
`)

// mustParse builds a template and parses both shared partials
// (previewCard + listPartial) plus name's own body. Every callable
// template shares the same partials, so a single Lookup-and-Execute
// path works for the page, the post-mutation fragment, the
// standalone preview, the form, and the form-error fragment.
func mustParse(name, body string) *template.Template {
	t := template.New(name).Funcs(funcs)
	template.Must(t.Parse(previewPartialSrc))
	template.Must(t.Parse(listPartialSrc))
	template.Must(t.Parse(body))
	return t
}
