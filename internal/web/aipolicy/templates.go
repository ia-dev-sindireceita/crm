// Package aipolicy ships the HTMX admin UI for /settings/ai-policy.
//
// SIN-63945 / UX-F8 migrates this surface onto the shell.Layout (F1
// design-system foundation, SIN-63935) so the page-chrome (top-bar,
// nav, user-menu, CSP-nonced tenant theme) is shared with every other
// authenticated route. The screen also adds:
//
//   - a sticky right-rail "Política efetiva" precedence preview that
//     updates within 400ms while the operator edits a policy (Doherty);
//   - a closed 3-tier LGPD field selector (Green/Yellow/Red) wired to
//     the SecurityEngineer-authored lgpd-field-spec (SIN-63945 doc);
//   - a sticky inline LGPD banner whose verbatim PT-BR text comes from
//     the SE spec (no rewording);
//   - audit emission of ai_policy.field_opt_in.<name> events through
//     the existing RecordingRepository decorator (SIN-62353 wiring).
//
// Templates follow the same pattern as internal/web/funnel: a single
// shell.MustParse call composes the chrome with the content blocks
// declared in page.html; per-feature partials (form, error,
// listPartial, previewCard, precedencePanel, fieldTier, lgpdBanner)
// are grafted on via AddParseTree.
package aipolicy

import (
	"embed"
	"html/template"
	"io"
	"reflect"
	"time"

	"github.com/pericles-luz/crm/internal/aipolicy"
	"github.com/pericles-luz/crm/internal/web/shell"
)

//go:embed page.html
var contentFS embed.FS

// pageData is the full-page view-model. The TenantName / CSPNonce /
// TenantThemeStyle fields are read by the shell.Layout reflection
// helpers (shellTenantName, shellCSPNonce, shellTenantThemeStyle) so
// the chrome and content share one struct. SIN-63945 layers the
// precedence preview and the form's LGPD field selector on top of
// the existing list/form/preview view-model.
type pageData struct {
	// shell.Data fields (read by shell.Layout reflection helpers).
	TenantName       string
	TenantLogo       string
	UserDisplayName  string
	NavItems         []shell.NavItem
	UserMenuItems    []shell.UserMenuItem
	CSRFToken        string
	CSPNonce         string
	TenantThemeStyle template.CSS

	// Page-specific content.
	GeneratedAt  string
	Rows         []rowData
	Preview      previewData
	Precedence   precedencePanelData
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
	ScopeType        string
	ScopeID          string
	Model            string
	Tone             string
	Language         string
	AIEnabled        bool
	Anonymize        bool
	OptIn            bool
	StructuredFields []string
	UpdatedAt        string
}

// formData seeds the new and edit form. IsNew flips the form action
// + method between POST (create) and PATCH (update). The Fields slice
// drives the 3-tier LGPD selector (SIN-63945 / UX-F8); each entry
// carries the catalog row + the current "enabled" state. ShowBanner
// flags the sticky inline LGPD alert when at least one Yellow tier
// is enabled.
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
	Fields           []fieldRowView
	ShowBanner       bool
	BannerFirstSeen  bool
}

// fieldRowView is one row of the LGPD field selector. Tier maps to a
// CSS modifier (field-tier--green / --yellow / --red) and to the
// disabled/aria-disabled state. LabelPT is the screen-reader-friendly
// human label; Name is the data-field machine identifier persisted in
// Policy.StructuredFields.
type fieldRowView struct {
	Name        string
	Tier        string
	LabelPT     string
	LegalAnchor string
	PromptForm  string
	Enabled     bool
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

// precedencePanelData drives the right-rail "Política efetiva" panel.
// PerField is the per-field precedence table (which scope "won" each
// row). PromptLines is the tokenised-preview pane Q3 ratified.
// ResolvedAt is the absolute Doherty timestamp the panel header
// flashes on swap.
type precedencePanelData struct {
	Mode           string
	ConversationID string
	Policy         aipolicy.Policy
	Source         string
	PerField       []precedenceFieldRow
	PromptLines    []string
	ResolvedAt     string
	Empty          bool
	EmptyMessage   string
}

// precedenceFieldRow is one (campo, valor, veio de) tuple shown in
// the precedence table. SourceLabel is pre-rendered so the template
// stays free of switch logic.
type precedenceFieldRow struct {
	Name        string
	Value       string
	SourceLabel string
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
	// aipolicyTenantName / aipolicyGeneratedAt are reflection-based
	// accessors so the page.html "content" block stays renderable from
	// the templates_csp_nonce_test / templates_theme_test fixtures
	// whose view struct only seeds TenantThemeStyle + CSPNonce.
	"aipolicyTenantName":  aipolicyViewString("TenantName", "CRM"),
	"aipolicyGeneratedAt": aipolicyViewString("GeneratedAt", time.Now().UTC().Format(time.RFC3339)),
}

// aipolicyViewString builds a reflection-based string accessor with a
// fallback. The shell helpers in internal/web/shell own the same
// pattern; we duplicate locally so the package owns its own helpers
// and tests can stub them without reaching across packages.
func aipolicyViewString(field, fallback string) func(any) string {
	return func(data any) string {
		v, ok := aipolicyViewUnwrap(data)
		if !ok {
			return fallback
		}
		f := v.FieldByName(field)
		if !f.IsValid() {
			return fallback
		}
		if s, ok := f.Interface().(string); ok {
			if s == "" {
				return fallback
			}
			return s
		}
		return fallback
	}
}

func aipolicyViewUnwrap(data any) (reflect.Value, bool) {
	if data == nil {
		return reflect.Value{}, false
	}
	v := reflect.ValueOf(data)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return reflect.Value{}, false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	return v, true
}

// previewPartialSrc is the cascade preview card markup. Defined as a
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
      <th scope="col">Campos amarelos</th>
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
      <td>
        {{- if .StructuredFields -}}
        <code>{{range $i, $f := .StructuredFields}}{{if $i}}, {{end}}{{$f}}{{end}}</code>
        {{- else -}}
        <small>—</small>
        {{- end -}}
      </td>
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

// lgpdBannerSrc is the sticky inline LGPD alert defined verbatim in
// the SecurityEngineer spec ([`lgpd-field-spec`](/SIN/issues/SIN-63945
// #document-lgpd-field-spec) §"Banner text"). The text is reproduced
// here byte-for-byte; the snapshot test in templates_banner_test.go
// rejects any drift.
//
// role attribute: SE spec §"Visual indicator requirements" requires
// role="alert" on first appearance per session. We render the banner
// with role="alert" when .BannerFirstSeen is true and role="status"
// on subsequent HTMX swaps within the same session.
const lgpdBannerSrc = `
{{define "aipolicy.lgpdBanner"}}
<aside id="lgpd-yellow-banner"
       class="aipolicy-lgpd-banner lgpd-banner"
       role="{{if .BannerFirstSeen}}alert{{else}}status{{end}}"
       aria-live="polite"
       data-testid="aipolicy-lgpd-banner">
  <header class="aipolicy-lgpd-banner__header">
    <span aria-hidden="true" class="aipolicy-lgpd-banner__icon">⚠️</span>
    <strong>Você está habilitando o envio de PII para a LLM (OpenRouter).</strong>
  </header>
  <div class="aipolicy-lgpd-banner__body">
    <p>Os campos amarelos selecionados serão enviados <strong>tokenizados</strong> (por exemplo: <code>[PII:EMAIL]</code>, <code>[PII:PHONE]</code>, <code>[PII:CNPJ]</code>) — a LLM nunca recebe o valor em texto claro. Mesmo assim, a presença do campo na requisição constitui tratamento de dado pessoal (LGPD Art. 5 I, Art. 7 II).</p>
    <p>Base legal aplicada: <strong>consentimento do controlador titular do dado</strong> (este tenant), conforme política configurada. O titular do dado pessoal (cliente final) é informado em <a href="/settings/privacy">/settings/privacy</a> sobre a divulgação do OpenRouter como sub-processador.</p>
    <p>Você pode revogar esta opção a qualquer momento; novas chamadas deixarão de incluir os campos. Conversas já enviadas permanecem nos logs do OpenRouter conforme o DPA.</p>
  </div>
  <footer class="aipolicy-lgpd-banner__links">
    <a href="/settings/privacy">Política de privacidade do tenant</a>
  </footer>
</aside>
{{end}}
`

// fieldTierSrc is the 3-tier LGPD field selector. Each row carries
// the data-field + data-tier attributes the spec mandates and renders
// icon + label + checkbox per WCAG 2.1 AA (color is never the only
// cue). Red rows render disabled + aria-disabled.
const fieldTierSrc = `
{{define "aipolicy.fieldTier"}}
<div class="field-tier-group" role="group" aria-labelledby="field-tier-heading">
  <h3 id="field-tier-heading">Campos enviados à LLM</h3>
  <p class="field-tier-group__hint">
    Campos verdes são enviados em texto claro; amarelos exigem opt-in e são
    tokenizados; vermelhos nunca são enviados (bloqueio LGPD).
  </p>
  {{- if .ShowBanner}}
  {{template "aipolicy.lgpdBanner" .}}
  {{- end}}
  <ul class="field-tier-list" role="list">
    {{- range .Fields}}
    <li class="field-tier field-tier--{{.Tier}}"
        data-field="{{.Name}}"
        data-tier="{{.Tier}}">
      <label class="field-tier__row">
        {{- if eq .Tier "red"}}
        <input type="checkbox"
               name="structured_fields"
               value="{{.Name}}"
               class="field-tier__check"
               disabled
               aria-disabled="true">
        <span class="field-tier__icon field-tier__icon--lock" aria-hidden="true">🔒</span>
        {{- else if eq .Tier "yellow"}}
        <input type="checkbox"
               name="structured_fields"
               value="{{.Name}}"
               class="field-tier__check"
               {{if .Enabled}}checked{{end}}>
        <span class="field-tier__icon field-tier__icon--warning" aria-hidden="true">⚠️</span>
        {{- else}}
        <input type="checkbox"
               name="structured_fields"
               value="{{.Name}}"
               class="field-tier__check"
               checked
               disabled
               aria-disabled="true">
        <span class="field-tier__icon field-tier__icon--check" aria-hidden="true">✅</span>
        {{- end}}
        <span class="field-tier__name">{{.LabelPT}}</span>
        <span class="field-tier__label">
          {{- if eq .Tier "green"}}Permitido (sempre enviado)
          {{- else if eq .Tier "yellow"}}PII — requer opt-in
          {{- else}}Bloqueado por LGPD{{end -}}
        </span>
        <small class="field-tier__anchor">{{.LegalAnchor}}</small>
      </label>
      {{- if eq .Tier "red"}}
      <span class="field-tier__tooltip" data-testid="field-tier-red-tooltip">🔒 <strong>Campo bloqueado por LGPD Art. 5 II / Art. 11</strong> (dado pessoal sensível). Este campo nunca é enviado à LLM. Para alterar a classificação, é necessário parecer jurídico e ADR.</span>
      {{- end}}
    </li>
    {{- end}}
  </ul>
</div>
{{end}}
`

// precedencePanelSrc is the right-rail "Política efetiva" panel
// returned by GET /settings/ai-policy/precedence. Its <pre> block
// shows the tokenised preview lines so the operator sees the exact
// "Próximo prompt incluirá" artefact (Q3 ratified). Doherty target:
// <400ms p95.
const precedencePanelSrc = `
{{define "aipolicy.precedencePanel"}}
<section class="aipolicy-precedence" aria-live="polite" data-testid="aipolicy-precedence-panel">
  {{- if .Empty}}
  <p class="aipolicy-precedence__empty">{{.EmptyMessage}}</p>
  {{- else}}
  <header class="aipolicy-precedence__head">
    <h3>Política efetiva</h3>
    {{- if .ResolvedAt}}<small class="aipolicy-precedence__doherty">Atualizado em <time>{{.ResolvedAt}}</time></small>{{end}}
  </header>
  <table class="aipolicy-precedence__table">
    <caption class="visually-hidden">Origem da regra para cada campo da política efetiva</caption>
    <thead><tr><th scope="col">Campo</th><th scope="col">Valor</th><th scope="col">Veio de</th></tr></thead>
    <tbody>
      {{- range .PerField}}
      <tr><th scope="row">{{.Name}}</th><td>{{.Value}}</td><td>{{.SourceLabel}}</td></tr>
      {{- end}}
    </tbody>
  </table>
  <section class="aipolicy-precedence__prompt">
    <h4>Próximo prompt incluirá</h4>
    <pre class="aipolicy-precedence__pre"><code>{{- range .PromptLines}}
{{.}}
{{- end}}</code></pre>
    <p class="aipolicy-precedence__note">
      <small>Campos amarelos ativos são enviados <strong>tokenizados</strong>; o valor real
      nunca sai do CRM. Campos vermelhos nunca são enviados.</small>
    </p>
  </section>
  {{- end}}
</section>
{{end}}
`

// formSrc is the new/edit form. POST writes the full Policy + the
// structured_fields slice; PATCH updates it. The form hosts the LGPD
// banner + field-tier selector inline (no separate fields endpoint —
// the banner state is server-rendered from the same upsert response).
//
// The Doherty live-preview hook (hx-trigger="keyup changed delay:300ms")
// fires the precedence GET on every keystroke; the preview endpoint is
// side-effect-free (R3 regression).
const formSrc = `
{{define "aipolicy.form"}}
<form id="aipolicy-form-element"
      class="aipolicy-form"
      hx-{{.Method}}="{{.Action}}"
      hx-target="#aipolicy-list"
      hx-swap="innerHTML"
      hx-trigger="submit, keyup changed delay:300ms"
      hx-get-on-keyup="/settings/ai-policy/precedence"
      data-testid="aipolicy-form">
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
      Tenant deu opt-in explícito (LGPD) — legado
    </label>

    {{template "aipolicy.fieldTier" .}}

    <button type="submit">{{if .IsNew}}Criar policy{{else}}Atualizar policy{{end}}</button>
  </fieldset>
</form>
{{end}}
`

// errorPartialSrc is the 422 response rendered when form validation
// fails. The form re-render path uses the parent form (HTMX swaps
// only this fragment back) so the user keeps their inputs while the
// problem is surfaced.
const errorPartialSrc = `
{{define "aipolicy.error"}}
<aside class="aipolicy-form-error" role="alert" data-field="{{.Field}}">
  <strong>Erro no campo {{.Field}}:</strong> {{.Message}}
</aside>
{{end}}
`

// pagePartialsSrc is every {{define}} block the page tree needs.
// Parsed up-front so each handler-facing *template.Template can look
// the partial up by name regardless of which entry point seeded it.
var pagePartialsSrc = previewPartialSrc + listPartialSrc + lgpdBannerSrc + fieldTierSrc + precedencePanelSrc + formSrc + errorPartialSrc

// shellFuncs merges the local funcs with shell.BaseFuncs so the
// caller-supplied helpers see the same map as the shell layout. The
// funcs map is the per-feature view, and the shell helpers must win
// only when a key collides — but no key collides today.
func shellFuncs() template.FuncMap {
	merged := template.FuncMap{}
	for k, v := range funcs {
		merged[k] = v
	}
	return merged
}

// pageTreeTmpl is the shell.Layout-composed page tree. It carries the
// chrome (top-bar, nav, user-menu, CSP-nonced tenant theme) plus the
// content block from page.html plus every partial in pagePartialsSrc.
// SIN-63945 / UX-F8.
var pageTreeTmpl = func() *template.Template {
	t := shell.MustParse(shellFuncs(), contentFS, "page.html")
	template.Must(t.Parse(pagePartialsSrc))
	return t
}()

// pageTmpl is the entry point used by full-page renders AND by the
// SIN-63275 templates_csp_nonce_test / templates_theme_test. It maps to
// the shell layout sub-tree so the existing "<style id=\"tenant-theme\"
// nonce=\"…\">" assertions match the migrated chrome.
var pageTmpl = pageTreeTmpl.Lookup("layout")

// listPartialTmpl is the table + preview card returned to HTMX after
// any mutation. Executes the listPartial define directly so HTMX
// clients get just the partial fragment.
var listPartialTmpl = pageTreeTmpl.Lookup("aipolicy.listPartial")

// previewTmpl is the standalone preview card response for the HTMX
// preview-form swap (legacy GET /settings/ai-policy/preview).
var previewTmpl = pageTreeTmpl.Lookup("aipolicy.previewCard")

// formTmpl is the new/edit form. HTMX submits with the configured
// method; the response replaces #aipolicy-list (the table) so the
// new/updated row appears without a full reload.
var formTmpl = pageTreeTmpl.Lookup("aipolicy.form")

// errorPartialTmpl is the 422 response rendered when form validation
// fails.
var errorPartialTmpl = pageTreeTmpl.Lookup("aipolicy.error")

// precedencePanelTmpl is the right-rail panel rendered by GET
// /settings/ai-policy/precedence and embedded in the full page.
var precedencePanelTmpl = pageTreeTmpl.Lookup("aipolicy.precedencePanel")

// fieldTierTmpl is the LGPD field selector partial returned by the
// per-field toggle endpoint when we want to refresh just the field
// group (OOB swap target).
var fieldTierTmpl = pageTreeTmpl.Lookup("aipolicy.fieldTier")

// lgpdBannerTmpl is the standalone banner template the OOB swap can
// emit when the Yellow field set changes.
var lgpdBannerTmpl = pageTreeTmpl.Lookup("aipolicy.lgpdBanner")

func init() {
	// Prime html/template's lazy escaper now, before any concurrent
	// goroutine can race on the first Execute call (same rationale as
	// web/inbox templates.go init prewarm — html/template AddParseTree
	// race fixed in bc30fb1).
	for _, t := range []*template.Template{
		pageTreeTmpl,
		pageTmpl,
		listPartialTmpl,
		previewTmpl,
		formTmpl,
		errorPartialTmpl,
		precedencePanelTmpl,
		fieldTierTmpl,
		lgpdBannerTmpl,
	} {
		if t != nil {
			_ = t.Execute(io.Discard, nil)
		}
	}
}
