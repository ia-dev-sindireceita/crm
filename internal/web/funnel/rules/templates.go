package rules

import (
	"html/template"
	"io"

	"github.com/pericles-luz/crm/internal/web/icon"
)

// funcs is the shared funcmap every template in this package parses
// against. Helpers MUST be registered before Parse — html/template
// rejects unknown function names at Parse time when invoked via the
// pipeline syntax `{{template "x" mkY .}}`. Hence the helpers are
// declared here as package-level variables wired into the FuncMap.
//
// SIN-65098 / Pitho Tranche C3 — merges the {{icon}} helper so the
// editor chrome (new-rule / back buttons) renders inline Lucide SVGs
// instead of typographic glyphs.
var funcs = buildFuncs()

func buildFuncs() template.FuncMap {
	f := template.FuncMap{
		"mkTriggerFieldsViewFromForm": mkTriggerFieldsViewFromForm,
		"mkActionFieldsViewFromForm":  mkActionFieldsViewFromForm,
	}
	for k, v := range icon.FuncMap() {
		f[k] = v
	}
	return f
}

// listLayoutTmpl is the editor shell. The table body lives in its own
// partial (listRowsTmpl) so create/update responses can swap the rows
// back inline without re-rendering the full page.
var listLayoutTmpl = template.Must(template.New("funnelrules.list").Funcs(funcs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Regras de funil</title>
  {{.CSRFMeta}}
  <link rel="stylesheet" href="/static/css/tokens.css">
  <link rel="stylesheet" href="/static/css/components.css">
  <link rel="stylesheet" href="/static/css/funnel-rules.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="funnel-rules-shell" role="main" aria-label="Regras de funil">
    <header class="funnel-rules-shell__header">
      <h1>Regras de funil</h1>
      <nav class="funnel-rules-shell__actions" aria-label="Ações de regra">
        <a class="funnel-rules-shell__new btn btn--primary"
           hx-get="/funnel/rules/new"
           hx-target="#funnel-rules-table"
           hx-swap="innerHTML"
           href="/funnel/rules/new">{{icon "plus" 16}} Nova regra</a>
      </nav>
    </header>
    <section class="funnel-rules-filter" aria-label="Filtrar por escopo">
      <form hx-get="/funnel/rules"
            hx-target="#funnel-rules-table"
            hx-swap="innerHTML"
            hx-trigger="change">
        <label>
          Escopo
          <select name="scope" aria-label="Escopo">
            <option value=""        {{if eq .ScopeFilter ""}}selected{{end}}>Todos</option>
            <option value="channel" {{if eq .ScopeFilter "channel"}}selected{{end}}>Canal</option>
            <option value="team"    {{if eq .ScopeFilter "team"}}selected{{end}}>Equipe</option>
            <option value="tenant"  {{if eq .ScopeFilter "tenant"}}selected{{end}}>Tenant</option>
          </select>
        </label>
      </form>
    </section>
    <section class="funnel-rules-table-wrap" aria-live="polite">
      <div id="funnel-rules-table">
        {{template "rows" .}}
      </div>
    </section>
    <section class="funnel-rules-preview" aria-label="Pré-visualização do cascade">
      <h2>Cascade preview</h2>
      <p class="funnel-rules-preview__hint">
        Escolha um escopo e veja qual regra venceria um evento entrando agora.
      </p>
      <form class="funnel-rules-preview__form"
            hx-get="/funnel/rules/preview"
            hx-target="#funnel-rules-preview-result"
            hx-swap="innerHTML"
            hx-trigger="change,keyup changed delay:300ms">
        <label>
          Canal
          <input name="channel" type="text" placeholder="webchat, whatsapp, …" maxlength="80">
        </label>
        <label>
          team_id (uuid)
          <input name="team_id" type="text" placeholder="00000000-0000-0000-0000-000000000000" maxlength="40">
        </label>
      </form>
      <div id="funnel-rules-preview-result" aria-live="polite"></div>
    </section>
    <footer class="funnel-rules-shell__footer">
      <small>Atualizado em {{.Generated}}</small>
    </footer>
  </main>
</body>
</html>
`))

// listRowsTmpl is the rows partial. Both the layout's initial render
// and create/update/list responses produce this same shape so HTMX can
// swap them inline.
var listRowsTmpl = template.Must(template.New("rows").Funcs(funcs).Parse(`<table class="funnel-rules-table" role="grid" aria-label="Lista de regras">
  <thead>
    <tr>
      <th scope="col">Nome</th>
      <th scope="col">Escopo</th>
      <th scope="col">Gatilho</th>
      <th scope="col">Ação</th>
      <th scope="col">Estado</th>
      <th scope="col">Ações</th>
    </tr>
  </thead>
  <tbody>
{{- range .Rows}}
    {{template "rule-row" .}}
{{- else}}
    <tr><td colspan="6" class="funnel-rules-empty">Nenhuma regra cadastrada.</td></tr>
{{- end}}
  </tbody>
</table>
`))

// rowTmpl is the per-row partial used both by the list and by the
// toggle response (HTMX swaps the row in place).
var rowTmpl = template.Must(template.New("rule-row").Funcs(funcs).Parse(`<tr id="rule-row-{{.ID}}" class="funnel-rules-row {{if not .Enabled}}funnel-rules-row--disabled{{end}}">
      <th scope="row">{{.Name}}</th>
      <td>{{.ScopeLabel}}</td>
      <td>
        <span class="funnel-rules-row__trigger-type">{{.TriggerType}}</span>
        {{if .TriggerInfo}}<small class="funnel-rules-row__trigger-info">{{.TriggerInfo}}</small>{{end}}
      </td>
      <td>
        <span class="funnel-rules-row__action-type">{{.ActionType}}</span>
        {{if .ActionInfo}}<small class="funnel-rules-row__action-info">{{.ActionInfo}}</small>{{end}}
      </td>
      <td><span class="funnel-rules-row__enabled funnel-rules-row__enabled--{{if .Enabled}}on{{else}}off{{end}}">{{.EnabledLabel}}</span></td>
      <td class="funnel-rules-row__actions">
        <button type="button"
                class="funnel-rules-row__toggle btn btn--sm"
                hx-patch="/funnel/rules/{{.ID}}/toggle"
                hx-target="#rule-row-{{.ID}}"
                hx-swap="outerHTML"
                aria-label="Alternar regra {{.Name}}">{{if .Enabled}}desativar{{else}}ativar{{end}}</button>
        <a class="funnel-rules-row__edit btn btn--sm btn--ghost"
           hx-get="/funnel/rules/{{.ID}}/edit"
           hx-target="#funnel-rules-table"
           hx-swap="innerHTML"
           href="/funnel/rules/{{.ID}}/edit"
           aria-label="Editar regra {{.Name}}">editar</a>
        <button type="button"
                class="funnel-rules-row__delete btn btn--sm btn--danger"
                hx-delete="/funnel/rules/{{.ID}}"
                hx-target="#rule-row-{{.ID}}"
                hx-swap="outerHTML"
                hx-confirm="Excluir esta regra?"
                aria-label="Excluir regra {{.Name}}">excluir</button>
      </td>
    </tr>
`))

// formLayoutTmpl is the create/edit form (full-page shell). Submission
// targets the rules table partial; on 422 the form re-renders with the
// inline error.
var formLayoutTmpl = template.Must(template.New("funnelrules.form").Funcs(funcs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>{{if eq (printf "%s" .Mode) "edit"}}Editar regra{{else}}Nova regra{{end}}</title>
  {{.CSRFMeta}}
  <link rel="stylesheet" href="/static/css/tokens.css">
  <link rel="stylesheet" href="/static/css/components.css">
  <link rel="stylesheet" href="/static/css/funnel-rules.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="funnel-rules-shell" role="main" aria-label="Formulário de regra">
    <header class="funnel-rules-shell__header">
      <h1>{{if eq (printf "%s" .Mode) "edit"}}Editar regra{{else}}Nova regra{{end}}</h1>
      <nav class="funnel-rules-shell__actions">
        <a class="funnel-rules-shell__back btn btn--sm btn--ghost" href="/funnel/rules" hx-get="/funnel/rules" hx-target="body" hx-swap="outerHTML">{{icon "chevron-left" 16}} Voltar</a>
      </nav>
    </header>
    <form class="funnel-rules-form"
          {{if eq (printf "%s" .Mode) "edit"}}hx-patch="/funnel/rules/{{.ID}}"{{else}}hx-post="/funnel/rules"{{end}}
          hx-target="#funnel-rules-table"
          hx-swap="innerHTML">
      {{- if not .Error.IsZero}}
      <div class="funnel-rules-form__alert alert alert--danger" role="alert">{{.Error.Message}}</div>
      {{- end}}
      <label>
        Nome
        <input name="name" type="text" required maxlength="200" value="{{.Input.Name}}"
               aria-invalid="{{if eq .Error.Field "name"}}true{{else}}false{{end}}">
      </label>
      <fieldset class="funnel-rules-form__scope">
        <legend>Escopo</legend>
        <label><input type="radio" name="scope" value="channel" {{if eq .Input.Scope "channel"}}checked{{end}}> Canal específico</label>
        <label><input type="radio" name="scope" value="team"    {{if eq .Input.Scope "team"}}checked{{end}}> Equipe específica</label>
        <label><input type="radio" name="scope" value="tenant"  {{if eq .Input.Scope "tenant"}}checked{{end}}> Tenant (padrão)</label>
      </fieldset>
      <label>
        Canal (preencha se escopo = canal)
        <input name="channel" type="text" maxlength="80" value="{{.Input.Channel}}"
               placeholder="webchat, whatsapp, instagram, …"
               aria-invalid="{{if eq .Error.Field "channel"}}true{{else}}false{{end}}">
      </label>
      <label>
        team_id (preencha se escopo = equipe; uuid)
        <input name="team_id" type="text" maxlength="40" value="{{.Input.TeamID}}"
               placeholder="00000000-0000-0000-0000-000000000000"
               aria-invalid="{{if eq .Error.Field "team_id"}}true{{else}}false{{end}}">
      </label>
      <label>
        Gatilho
        <select name="trigger_type"
                hx-get="/funnel/rules/trigger-fields"
                hx-include="closest form"
                hx-target="#funnel-rules-trigger-fields"
                hx-swap="innerHTML"
                hx-trigger="change"
                hx-vals='js:{type: event.target.value}'
                aria-invalid="{{if eq .Error.Field "trigger_type"}}true{{else}}false{{end}}">
          {{range .TriggerOptions}}
          <option value="{{.Value}}" {{if eq $.Input.TriggerType .Value}}selected{{end}}>{{.Label}}</option>
          {{end}}
        </select>
      </label>
      <div id="funnel-rules-trigger-fields">
        {{template "trigger-fields" mkTriggerFieldsViewFromForm .Input}}
      </div>
      <label>
        Ação
        <select name="action_type"
                hx-get="/funnel/rules/action-fields"
                hx-include="closest form"
                hx-target="#funnel-rules-action-fields"
                hx-swap="innerHTML"
                hx-trigger="change"
                hx-vals='js:{type: event.target.value}'
                aria-invalid="{{if eq .Error.Field "action_type"}}true{{else}}false{{end}}">
          {{range .ActionOptions}}
          <option value="{{.Value}}" {{if eq $.Input.ActionType .Value}}selected{{end}}>{{.Label}}</option>
          {{end}}
        </select>
      </label>
      <div id="funnel-rules-action-fields">
        {{template "action-fields" mkActionFieldsViewFromForm .Input}}
      </div>
      <label class="funnel-rules-form__enabled">
        <input type="checkbox" name="enabled" {{if .Input.Enabled}}checked{{end}}>
        Ativa
      </label>
      <div class="funnel-rules-form__actions">
        <button type="submit" class="funnel-rules-form__submit btn btn--primary">{{if eq (printf "%s" .Mode) "edit"}}Salvar alterações{{else}}Criar regra{{end}}</button>
      </div>
    </form>
  </main>
</body>
</html>
`))

// triggerFieldsTmpl renders the per-trigger-type input set. The select
// in the parent form fires hx-get on /funnel/rules/trigger-fields with
// type=<value> and HTMX swaps this partial in place.
var triggerFieldsTmpl = template.Must(template.New("trigger-fields").Funcs(funcs).Parse(`{{if eq .Type "message_contains"}}
<label>
  Frase
  <input name="trigger_phrase" type="text" maxlength="512" value="{{.Input.TriggerCfg.Phrase}}"
         placeholder="ex: preço">
</label>
{{else if eq .Type "campaign_click"}}
<label>
  campaign_id (uuid — opcional se informar slug)
  <input name="trigger_campaign_id" type="text" maxlength="40" value="{{.Input.TriggerCfg.CampaignID}}">
</label>
<label>
  slug (opcional se informar campaign_id)
  <input name="trigger_slug" type="text" maxlength="80" value="{{.Input.TriggerCfg.Slug}}">
</label>
{{else if eq .Type "message_keyword_regex"}}
<label>
  Regex (case-sensitive)
  <input name="trigger_regex" type="text" maxlength="512" value="{{.Input.TriggerCfg.Regex}}"
         placeholder="ex: NF-\d+">
</label>
{{else}}
<p class="funnel-rules-form__hint">Selecione um tipo de gatilho conhecido.</p>
{{end}}
`))

// actionFieldsTmpl renders the per-action-type input set.
var actionFieldsTmpl = template.Must(template.New("action-fields").Funcs(funcs).Parse(`{{if eq .Type "move_to_stage"}}
<label>
  Estágio destino (key)
  <input name="action_stage_key" type="text" maxlength="80" value="{{.Input.ActionCfg.StageKey}}"
         placeholder="ex: qualificando, ganho, perdido">
</label>
{{else}}
<p class="funnel-rules-form__hint">Selecione um tipo de ação conhecido.</p>
{{end}}
`))

// previewTmpl renders the cascade-resolver result for the requested
// scope. The list reads top-to-bottom in the cascade order so the
// reader sees which rule wins first.
var previewTmpl = template.Must(template.New("preview").Funcs(funcs).Parse(`{{if .Error}}
<p class="funnel-rules-preview__error" role="alert">{{.Error}}</p>
{{else if not .Resolved}}
<p class="funnel-rules-preview__empty">Nenhuma regra ativa para este escopo.</p>
{{else}}
<ol class="funnel-rules-preview__list" role="list" aria-label="Regras efetivas">
{{range .Resolved}}
  <li class="funnel-rules-preview__item">
    <span class="funnel-rules-preview__scope">{{.Scope}}</span>
    <span class="funnel-rules-preview__name">{{.Name}}</span>
    <span class="funnel-rules-preview__sep">·</span>
    <span class="funnel-rules-preview__trigger">{{.Trigger}}{{if .TriggerInfo}} ({{.TriggerInfo}}){{end}}</span>
    <span class="funnel-rules-preview__arrow">→</span>
    <span class="funnel-rules-preview__action">{{.Action}}{{if .ActionInfo}} ({{.ActionInfo}}){{end}}</span>
  </li>
{{end}}
</ol>
{{end}}
`))

// mkTriggerFieldsViewFromForm + mkActionFieldsViewFromForm bundle the
// current form input into the shape the partial templates consume.
// Used inline by formLayoutTmpl so the initial render and the HTMX
// re-render share one template.
func mkTriggerFieldsViewFromForm(in formInput) triggerFieldsView {
	return triggerFieldsView{Type: in.TriggerType, Known: true, Input: in}
}

func mkActionFieldsViewFromForm(in formInput) actionFieldsView {
	return actionFieldsView{Type: in.ActionType, Known: true, Input: in}
}

func init() {
	// Register partials so the layouts can {{template "rows"}},
	// {{template "rule-row"}}, etc.
	if _, err := listLayoutTmpl.AddParseTree(listRowsTmpl.Name(), listRowsTmpl.Tree); err != nil {
		panic("web/funnel/rules: register rows partial: " + err.Error())
	}
	if _, err := listLayoutTmpl.AddParseTree(rowTmpl.Name(), rowTmpl.Tree); err != nil {
		panic("web/funnel/rules: register row partial under layout: " + err.Error())
	}
	if _, err := listRowsTmpl.AddParseTree(rowTmpl.Name(), rowTmpl.Tree); err != nil {
		panic("web/funnel/rules: register row partial under rows: " + err.Error())
	}
	if _, err := formLayoutTmpl.AddParseTree(triggerFieldsTmpl.Name(), triggerFieldsTmpl.Tree); err != nil {
		panic("web/funnel/rules: register trigger-fields partial: " + err.Error())
	}
	if _, err := formLayoutTmpl.AddParseTree(actionFieldsTmpl.Name(), actionFieldsTmpl.Tree); err != nil {
		panic("web/funnel/rules: register action-fields partial: " + err.Error())
	}

	// Prime html/template's lazy escaper now, before any concurrent
	// goroutine can race on the first Execute call (web/funnel /
	// web/inbox / web/campaigns init prewarm — reference memory
	// `html/template AddParseTree race (web/inbox)`).
	for _, t := range []*template.Template{
		listRowsTmpl, rowTmpl, listLayoutTmpl, formLayoutTmpl,
		triggerFieldsTmpl, actionFieldsTmpl, previewTmpl,
	} {
		_ = t.Execute(io.Discard, nil)
	}
}
