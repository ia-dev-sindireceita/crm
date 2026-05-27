package master

import (
	"html/template"
	"io"

	"github.com/google/uuid"
)

// grantRequestsListData drives both the list layout and the partial
// returned for HTMX swaps. Keeping the shape identical across both
// means a partial swap renders byte-identical output to a full page
// re-render.
type grantRequestsListData struct {
	Requests         []GrantRequest
	Flash            string
	CSRFInput        template.HTML
	HXHeaders        template.HTMLAttr
	CSRFMeta         template.HTML
	TenantThemeStyle template.CSS
	CSPNonce         string
}

// grantRequestDetailData drives the request-detail page (GET
// /master/grants/requests/{id}) and the per-request swap after an
// approve/reject. FormError, when non-empty, renders an inline error
// banner above the form.
type grantRequestDetailData struct {
	Request          GrantRequest
	Flash            string
	FormError        string
	CSRFInput        template.HTML
	HXHeaders        template.HTMLAttr
	CSRFMeta         template.HTML
	TenantThemeStyle template.CSS
	CSPNonce         string
}

var grantRequestsTemplateFuncs = template.FuncMap{
	"formatGrantTime":   formatGrantTime,
	"grantKindLabel":    grantKindLabel,
	"isFreePeriod":      isFreePeriod,
	"isExtraTokens":     isExtraTokens,
	"int64ToStr":        int64ToStr,
	"grantRequestState": grantRequestStateLabel,
	"isAwaiting":        isAwaiting,
}

func grantRequestStateLabel(s GrantRequestState) string {
	switch s {
	case GrantRequestStateAwaiting:
		return "Aguardando aprovação"
	case GrantRequestStateApproved:
		return "Aprovada"
	case GrantRequestStateRejected:
		return "Rejeitada"
	default:
		return string(s)
	}
}

func isAwaiting(s GrantRequestState) bool {
	return s == GrantRequestStateAwaiting
}

// grantRequestsLayoutTmpl is the full page shell for GET
// /master/grants/requests. The list partial renders inside
// #grant-requests-panel so HTMX swaps land in that region.
var grantRequestsLayoutTmpl = template.Must(template.New("grant_requests.layout").Funcs(grantRequestsTemplateFuncs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Master · Aprovações 4-eyes</title>
  {{.CSRFMeta}}
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/master.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="master-shell" role="main" aria-labelledby="grant-requests-title">
    <header class="master-shell__header">
      <h1 id="grant-requests-title">Solicitações 4-eyes</h1>
      <p class="master-shell__hint">
        Grants acima do limite por grant ou por tenant (365d) aguardam
        a aprovação de um segundo master. O aprovador deve ser um usuário
        diferente do solicitante.
      </p>
      <p class="master-shell__crumb">
        <a href="/master/tenants">← Tenants</a>
      </p>
    </header>
    {{template "grant_requests_panel" .}}
  </main>
</body>
</html>
`))

// grantRequestsPanelTmpl is the list partial for HTMX swaps.
var grantRequestsPanelTmpl = template.Must(template.New("grant_requests_panel").Funcs(grantRequestsTemplateFuncs).Parse(`<section id="grant-requests-panel" class="master-grant-requests" aria-label="Solicitações de cortesia aguardando aprovação">
  {{if .Flash}}
  <div class="master-grant-requests__flash" role="status">{{.Flash}}</div>
  {{end}}

  <h2 class="master-grant-requests__title">Aguardando aprovação</h2>
  <table class="master-grant-requests__table" aria-describedby="grant-requests-title">
    <caption class="visually-hidden">Solicitações aguardando aprovação</caption>
    <thead>
      <tr>
        <th scope="col">Solicitada</th>
        <th scope="col">Tenant</th>
        <th scope="col">Tipo</th>
        <th scope="col">Valor</th>
        <th scope="col">Equivalência</th>
        <th scope="col">Solicitante</th>
        <th scope="col">Motivo</th>
        <th scope="col">Ação</th>
      </tr>
    </thead>
    <tbody>
      {{if .Requests}}
        {{range .Requests}}
        <tr class="master-grant-requests__row" id="grant-request-row-{{.ID}}" data-grant-request-id="{{.ID}}">
          <td><time datetime="{{.CreatedAt.Format "2006-01-02T15:04:05Z07:00"}}">{{formatGrantTime .CreatedAt}}</time></td>
          <td>{{.TenantID}}</td>
          <td>{{grantKindLabel .Kind}}</td>
          <td>
            {{if eq (printf "%s" .Kind) "free_subscription_period"}}{{.PeriodDays}} dias
            {{else if eq (printf "%s" .Kind) "extra_tokens"}}{{int64ToStr .Amount}} tokens
            {{else}}—{{end}}
          </td>
          <td>{{int64ToStr .CapEquivalence}} tokens</td>
          <td>{{.CreatedByID}}</td>
          <td><span class="master-grant-requests__reason">{{.Reason}}</span></td>
          <td><a class="master-grant-requests__review" href="/master/grants/requests/{{.ID}}">Revisar</a></td>
        </tr>
        {{end}}
      {{else}}
        <tr class="master-grant-requests__empty">
          <td colspan="8">Nenhuma solicitação aguardando aprovação.</td>
        </tr>
      {{end}}
    </tbody>
  </table>
</section>
`))

// grantRequestDetailLayoutTmpl is the full page shell for GET
// /master/grants/requests/{id}.
var grantRequestDetailLayoutTmpl = template.Must(template.New("grant_request_detail.layout").Funcs(grantRequestsTemplateFuncs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Master · Revisão 4-eyes</title>
  {{.CSRFMeta}}
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/master.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="master-shell" role="main" aria-labelledby="grant-request-detail-title">
    <header class="master-shell__header">
      <h1 id="grant-request-detail-title">Revisão de cortesia</h1>
      <p class="master-shell__crumb">
        <a href="/master/grants/requests">← Solicitações 4-eyes</a>
      </p>
    </header>
    {{template "grant_request_detail_panel" .}}
  </main>
</body>
</html>
`))

// grantRequestDetailPanelTmpl renders the review card + approve/reject
// forms.
var grantRequestDetailPanelTmpl = template.Must(template.New("grant_request_detail_panel").Funcs(grantRequestsTemplateFuncs).Parse(`<section id="grant-request-detail" class="master-grant-request-detail" aria-label="Detalhe da solicitação">
  {{if .Flash}}
  <div class="master-grant-request-detail__flash" role="status">{{.Flash}}</div>
  {{end}}
  {{if .FormError}}
  <div class="master-grant-request-detail__error" role="alert">{{.FormError}}</div>
  {{end}}

  <dl class="master-grant-request-detail__fields">
    <dt>ID externa</dt><dd>{{.Request.ExternalID}}</dd>
    <dt>Estado</dt><dd>{{grantRequestState .Request.State}}</dd>
    <dt>Tenant</dt><dd>{{.Request.TenantID}}</dd>
    <dt>Solicitante</dt><dd>{{.Request.CreatedByID}}</dd>
    <dt>Tipo</dt><dd>{{grantKindLabel .Request.Kind}}</dd>
    <dt>Valor</dt>
    <dd>
      {{if isFreePeriod (printf "%s" .Request.Kind)}}{{.Request.PeriodDays}} dias
      {{else if isExtraTokens (printf "%s" .Request.Kind)}}{{int64ToStr .Request.Amount}} tokens
      {{else}}—{{end}}
    </dd>
    <dt>Equivalência</dt><dd>{{int64ToStr .Request.CapEquivalence}} tokens</dd>
    <dt>Motivo</dt><dd>{{.Request.Reason}}</dd>
    <dt>Solicitada em</dt><dd><time datetime="{{.Request.CreatedAt.Format "2006-01-02T15:04:05Z07:00"}}">{{formatGrantTime .Request.CreatedAt}}</time></dd>
    {{if not (isAwaiting .Request.State)}}
    <dt>Decidida em</dt><dd><time datetime="{{.Request.DecidedAt.Format "2006-01-02T15:04:05Z07:00"}}">{{formatGrantTime .Request.DecidedAt}}</time></dd>
    <dt>Aprovador / Rejeitador</dt><dd>{{.Request.SecondApproverID}}</dd>
    {{end}}
  </dl>

  {{if isAwaiting .Request.State}}
  <div class="master-grant-request-detail__actions">
    <form class="master-grant-request-detail__approve"
          method="post"
          action="/master/grants/requests/{{.Request.ID}}/approve"
          hx-post="/master/grants/requests/{{.Request.ID}}/approve"
          hx-target="#grant-request-detail"
          hx-swap="outerHTML"
          aria-label="Aprovar solicitação">
      {{.CSRFInput}}
      <button type="submit" class="master-grant-request-detail__approve-submit">Aprovar</button>
    </form>
    <form class="master-grant-request-detail__reject"
          method="post"
          action="/master/grants/requests/{{.Request.ID}}/reject"
          hx-post="/master/grants/requests/{{.Request.ID}}/reject"
          hx-target="#grant-request-detail"
          hx-swap="outerHTML"
          aria-label="Rejeitar solicitação">
      {{.CSRFInput}}
      <button type="submit" class="master-grant-request-detail__reject-submit">Rejeitar</button>
    </form>
  </div>
  {{else}}
  <p class="master-grant-request-detail__decided">Esta solicitação já foi {{grantRequestState .Request.State}}.</p>
  {{end}}
</section>
`))

// noopRequestForView returns a sentinel placeholder used to prime the
// detail templates during init. The non-empty State field ensures the
// awaiting/decided branches both compile, and uuid.Nil is never a
// legitimate request id so an accidental render of the placeholder is
// trivial to spot in tests.
func noopRequestForView() GrantRequest {
	return GrantRequest{ID: uuid.Nil, State: GrantRequestStateAwaiting}
}

func init() {
	for parent, child := range map[*template.Template]*template.Template{
		grantRequestsLayoutTmpl:      grantRequestsPanelTmpl,
		grantRequestDetailLayoutTmpl: grantRequestDetailPanelTmpl,
	} {
		if _, err := parent.AddParseTree(child.Name(), child.Tree); err != nil {
			panic("web/master: register " + child.Name() + " in " + parent.Name() + ": " + err.Error())
		}
	}
	// Prime html/template's lazy escaper on every grant-request
	// template before any concurrent Execute can race on the first
	// call (SIN-62774 regression repro — see html_template_race
	// memory).
	for _, t := range []*template.Template{
		grantRequestsPanelTmpl, grantRequestsLayoutTmpl,
		grantRequestDetailPanelTmpl, grantRequestDetailLayoutTmpl,
	} {
		_ = t.Execute(io.Discard, grantRequestsListData{})
		_ = t.Execute(io.Discard, grantRequestDetailData{Request: noopRequestForView()})
	}
}
