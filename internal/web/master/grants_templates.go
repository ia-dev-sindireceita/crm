package master

import (
	"html/template"
	"io"
	"time"

	"github.com/google/uuid"
)

// grantsPageData drives both the full layout and the panel partial
// used by GET /master/tenants/{id}/grants/new and the HTMX swaps after
// POST. Keeping the data shape identical across templates means a
// partial swap renders byte-identical output to a full re-render.
type grantsPageData struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	Grants    []GrantRow
	Flash     string
	FormError string
	// Kind preserves the kind switch's UI state across re-renders
	// (e.g. a validation error on the extra_tokens variant should
	// leave the extra_tokens fields visible). Stored as string so the
	// template can compare directly.
	Kind      string
	CSRFInput template.HTML
	HXHeaders template.HTMLAttr
	CSRFMeta  template.HTML
}

var grantsTemplateFuncs = template.FuncMap{
	"formatGrantTime": formatGrantTime,
	"grantKindLabel":  grantKindLabel,
	"isFreePeriod":    isFreePeriod,
	"isExtraTokens":   isExtraTokens,
	"int64ToStr":      int64ToStr,
}

func formatGrantTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

func grantKindLabel(k GrantKind) string {
	switch k {
	case GrantKindFreeSubscriptionPeriod:
		return "Período grátis"
	case GrantKindExtraTokens:
		return "Tokens extras"
	default:
		return string(k)
	}
}

func isFreePeriod(kind string) bool {
	return kind == string(GrantKindFreeSubscriptionPeriod) || kind == ""
}

func isExtraTokens(kind string) bool {
	return kind == string(GrantKindExtraTokens)
}

func int64ToStr(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// grantsLayoutTmpl is the full page shell for GET /master/tenants/
// {id}/grants/new. The panel renders inside #grants-panel so HTMX
// swaps after POST land in that region.
var grantsLayoutTmpl = template.Must(template.New("grants.layout").Funcs(grantsTemplateFuncs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Master · Cortesias</title>
  {{.CSRFMeta}}
  <link rel="stylesheet" href="/static/css/master.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="master-shell" role="main" aria-labelledby="master-grants-title">
    <header class="master-shell__header">
      <h1 id="master-grants-title">Conceder cortesia</h1>
      <p class="master-shell__hint">
        Conceda um período de assinatura gratuito ou tokens extras para
        este tenant. As ações desta página são auditadas e exigem
        reverificação 2FA recente.
      </p>
      <p class="master-shell__crumb">
        <a href="/master/tenants">← Tenants</a>
      </p>
    </header>
    {{template "grants_panel" .}}
  </main>
</body>
</html>
`))

// grantsPanelTmpl is the partial returned for HTMX swaps on
// #grants-panel. It hosts both the create form and the historical
// grants list so a failed validation re-renders both with one round
// trip.
var grantsPanelTmpl = template.Must(template.New("grants_panel").Funcs(grantsTemplateFuncs).Parse(`<section id="grants-panel" class="master-grants" aria-label="Cortesias do tenant">
  {{if .Flash}}
  <div class="master-grants__flash" role="status">{{.Flash}}</div>
  {{end}}

  <form class="master-grants__create"
        method="post"
        action="/master/tenants/{{.TenantID}}/grants"
        hx-post="/master/tenants/{{.TenantID}}/grants"
        hx-target="#grants-panel"
        hx-swap="outerHTML"
        aria-labelledby="grants-create-title">
    <h2 id="grants-create-title">Nova cortesia</h2>
    {{if .FormError}}
    <p class="master-grants__form-error" role="alert">{{.FormError}}</p>
    {{end}}
    {{.CSRFInput}}
    <fieldset class="master-grants__kind">
      <legend>Tipo</legend>
      <label>
        <input type="radio" name="kind" value="free_subscription_period"
               data-grant-kind-toggle="free"
               {{if isFreePeriod .Kind}}checked{{end}}
               onclick="document.querySelectorAll('[data-grant-fields]').forEach(el=>el.hidden=el.dataset.grantFields!==this.value);">
        Período grátis (dias)
      </label>
      <label>
        <input type="radio" name="kind" value="extra_tokens"
               data-grant-kind-toggle="extra"
               {{if isExtraTokens .Kind}}checked{{end}}
               onclick="document.querySelectorAll('[data-grant-fields]').forEach(el=>el.hidden=el.dataset.grantFields!==this.value);">
        Tokens extras
      </label>
    </fieldset>

    <div class="master-grants__field"
         data-grant-fields="free_subscription_period"
         {{if isExtraTokens .Kind}}hidden{{end}}>
      <label for="grant-period-days">Período (dias)</label>
      <input id="grant-period-days" name="period_days" type="number" min="1" max="366" step="1" value="30">
    </div>

    <div class="master-grants__field"
         data-grant-fields="extra_tokens"
         {{if isFreePeriod .Kind}}hidden{{end}}>
      <label for="grant-amount">Quantidade de tokens</label>
      <input id="grant-amount" name="amount" type="number" min="1" step="1" value="100000">
    </div>

    <div class="master-grants__field">
      <label for="grant-reason">Motivo (mínimo 10 caracteres)</label>
      <textarea id="grant-reason" name="reason" minlength="10" required
                rows="3" placeholder="Justificativa para a cortesia..."></textarea>
    </div>

    <button type="submit" class="master-grants__create-submit">Conceder cortesia</button>
  </form>

  <h2 class="master-grants__history-title">Cortesias anteriores</h2>
  <table class="master-grants__table" aria-describedby="master-grants-title">
    <caption class="visually-hidden">Cortesias emitidas para este tenant</caption>
    <thead>
      <tr>
        <th scope="col">Tipo</th>
        <th scope="col">Valor</th>
        <th scope="col">Motivo</th>
        <th scope="col">Emitida</th>
        <th scope="col">Status</th>
        <th scope="col">Ações</th>
      </tr>
    </thead>
    <tbody>
      {{if .Grants}}
        {{range .Grants}}
        <tr class="master-grants__row" id="grant-row-{{.ID}}" data-grant-id="{{.ID}}">
          <td>{{grantKindLabel .Kind}}</td>
          <td>
            {{if eq (printf "%s" .Kind) "free_subscription_period"}}{{.PeriodDays}} dias
            {{else if eq (printf "%s" .Kind) "extra_tokens"}}{{int64ToStr .Amount}} tokens
            {{else}}—{{end}}
          </td>
          <td><span class="master-grants__reason">{{.Reason}}</span></td>
          <td><time datetime="{{.CreatedAt.Format "2006-01-02T15:04:05Z07:00"}}">{{formatGrantTime .CreatedAt}}</time></td>
          <td>
            {{if .Revoked}}<span data-grant-state="revoked" class="master-grants__pill master-grants__pill--revoked">Revogada</span>
            {{else if .Consumed}}<span data-grant-state="consumed" class="master-grants__pill master-grants__pill--consumed">Consumida</span>
            {{else}}<span data-grant-state="active" class="master-grants__pill master-grants__pill--active">Ativa</span>{{end}}
          </td>
          <td>
            {{if .IsRevocable}}
            <form class="master-grants__revoke"
                  method="post"
                  action="/master/grants/{{.ID}}/revoke"
                  hx-post="/master/grants/{{.ID}}/revoke"
                  hx-target="#grants-panel"
                  hx-swap="outerHTML"
                  aria-label="Revogar cortesia">
              {{$.CSRFInput}}
              <input type="hidden" name="tenant_id" value="{{$.TenantID}}">
              <label class="visually-hidden" for="revoke-reason-{{.ID}}">Motivo da revogação</label>
              <input id="revoke-reason-{{.ID}}" name="reason" type="text" minlength="10"
                     required placeholder="Motivo (≥ 10 chars)">
              <button type="submit" class="master-grants__revoke-submit">Revogar</button>
            </form>
            {{else if .Consumed}}
            <span class="master-grants__inactive">Já consumida — emita uma compensatória</span>
            {{else}}
            <span class="master-grants__inactive">Sem ações</span>
            {{end}}
          </td>
        </tr>
        {{end}}
      {{else}}
        <tr class="master-grants__empty">
          <td colspan="6">Nenhuma cortesia emitida para este tenant.</td>
        </tr>
      {{end}}
    </tbody>
  </table>
</section>
`))

func init() {
	if _, err := grantsLayoutTmpl.AddParseTree(grantsPanelTmpl.Name(), grantsPanelTmpl.Tree); err != nil {
		panic("web/master: register grants_panel in grants.layout: " + err.Error())
	}
	// Prime html/template's lazy escaper on every grants template
	// before any concurrent Execute can race on the first call
	// (SIN-62774 regression repro — see html_template_race memory).
	for _, t := range []*template.Template{grantsPanelTmpl, grantsLayoutTmpl} {
		_ = t.Execute(io.Discard, grantsPageData{})
	}
}
