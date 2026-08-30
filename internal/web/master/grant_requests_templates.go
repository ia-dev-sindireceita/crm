package master

import (
	"html/template"
	"io"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/web/shell"
)

// grantRequestsListData drives both the list layout and the partial
// returned for HTMX swaps. Keeping the shape identical across both
// means a partial swap renders byte-identical output to a full page
// re-render.
type grantRequestsListData struct {
	Requests            []GrantRequest
	Flash               string
	CSRFInput           template.HTML
	HXHeaders           template.HTMLAttr
	CSRFMeta            template.HTML
	TenantThemeStyle    template.CSS
	CSPNonce            string
	ActiveImpersonation *shell.ImpersonationContext

	// CurrentUserID is the master user viewing the inbox. Each row
	// renders the "Revisar →" link as disabled when CreatedByID
	// matches (defense in depth on top of the 422 backend response
	// per spec §4.3 / §10.4 #20).
	CurrentUserID uuid.UUID
}

// grantRequestDetailData drives the request-detail page (GET
// /master/grants/requests/{id}) and the per-request swap after an
// approve/reject. FormError, when non-empty, renders an inline error
// banner above the form.
type grantRequestDetailData struct {
	Request             GrantRequest
	Flash               string
	FormError           string
	CSRFInput           template.HTML
	HXHeaders           template.HTMLAttr
	CSRFMeta            template.HTML
	TenantThemeStyle    template.CSS
	CSPNonce            string
	ActiveImpersonation *shell.ImpersonationContext

	// CurrentUserID is the master user reviewing the request. When
	// it equals Request.CreatedByID the detail template renders the
	// self-approval guard (spec §4.3 + §10.4 #20) and suppresses the
	// approve/reject forms entirely.
	CurrentUserID uuid.UUID

	// ConfirmStage carries the "first-confirm received" cookie set by
	// POST /master/grants/requests/{id}/approve when the operator
	// clicks "Aprovar…". Empty → render the diff page; "confirm" →
	// render the confirm-twice modal pinned to the same request id.
	// This is what implements spec §4.4 / §10.4 #19 (confirm-twice)
	// without JS dependency — a pure server-rendered two-step dance.
	ConfirmStage string
}

var grantRequestsTemplateFuncs = template.FuncMap{
	"formatGrantTime":             formatGrantTime,
	"grantKindLabel":              grantKindLabel,
	"isFreePeriod":                isFreePeriod,
	"isExtraTokens":               isExtraTokens,
	"int64ToStr":                  int64ToStr,
	"grantRequestState":           grantRequestStateLabel,
	"isAwaiting":                  isAwaiting,
	"formatImpersonationISO":      formatImpersonationISO,
	"truncateImpersonationReason": truncateImpersonationReason,
	"csrfHiddenForToken":          csrfHiddenForToken,
	"eqUUID":                      eqUUID,
	"icon":                        iconSVG,
}

// eqUUID returns true when two uuids are equal. Used by templates to
// branch on self-approval guards without resorting to printf-eq dance.
func eqUUID(a, b uuid.UUID) bool { return a == b }

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
  <link rel="stylesheet" href="/static/css/tokens.css">
  <link rel="stylesheet" href="/static/css/master.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
  <script src="/static/js/impersonation-countdown.js" defer></script>
</head>
<body {{.HXHeaders}}{{with .ActiveImpersonation}} data-impersonating="true"{{end}}>
  {{template "shell_impersonation_banner" .}}
  {{template "shell_audit_feed_chip" .}}
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
          <td>
            <a class="master-grant-requests__review{{if eqUUID .CreatedByID $.CurrentUserID}} master-grant-requests__review--self{{end}}"
               href="/master/grants/requests/{{.ID}}"
               {{if eqUUID .CreatedByID $.CurrentUserID}}
                 aria-disabled="true"
                 data-self-row="true"
                 title="Você é o solicitante — aguarde outro master revisar (regra 4-eyes)."
               {{else}}
                 data-review-link="true"
               {{end}}>Revisar →</a>
          </td>
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

// (banner partial registered by registerImpersonationBanner at init)

// grantRequestDetailLayoutTmpl is the full page shell for GET
// /master/grants/requests/{id}.
var grantRequestDetailLayoutTmpl = template.Must(template.New("grant_request_detail.layout").Funcs(grantRequestsTemplateFuncs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Master · Revisão 4-eyes</title>
  {{.CSRFMeta}}
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/tokens.css">
  <link rel="stylesheet" href="/static/css/master.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
  <script src="/static/js/impersonation-countdown.js" defer></script>
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

// grantRequestDetailPanelTmpl renders the two-column diff (spec §9.8)
// with the confirm-twice approval modal (spec §4.4) and the self-
// approval guard (spec §4.3 + §10.4 #20).
var grantRequestDetailPanelTmpl = template.Must(template.New("grant_request_detail_panel").Funcs(grantRequestsTemplateFuncs).Parse(`<section id="grant-request-detail" class="master-grant-request-detail" aria-label="Detalhe da solicitação">
  {{if .Flash}}
  <div class="master-grant-request-detail__flash" role="status">{{.Flash}}</div>
  {{end}}
  {{if .FormError}}
  <div class="master-grant-request-detail__error" role="alert">{{.FormError}}</div>
  {{end}}

  <div class="master-grant-request-detail__columns">
    <article class="master-grant-request-detail__column master-grant-request-detail__column--requester"
             aria-labelledby="grant-request-requester-title">
      <span class="master-grant-request-detail__sigil">{{icon "lock"}} SOLICITANTE</span>
      <h2 id="grant-request-requester-title">Quem pediu</h2>
      <dl>
        <dt>Solicitante</dt><dd>{{.Request.CreatedByID}}</dd>
        <dt>Tenant</dt><dd>{{.Request.TenantID}}</dd>
        <dt>Tipo</dt><dd>{{grantKindLabel .Request.Kind}}</dd>
        <dt>Valor solicitado</dt>
        <dd>
          {{if isFreePeriod (printf "%s" .Request.Kind)}}{{.Request.PeriodDays}} dias
          {{else if isExtraTokens (printf "%s" .Request.Kind)}}{{int64ToStr .Request.Amount}} tokens
          {{else}}—{{end}}
        </dd>
        <dt>Solicitada em</dt>
        <dd><time datetime="{{.Request.CreatedAt.Format "2006-01-02T15:04:05Z07:00"}}">{{formatGrantTime .Request.CreatedAt}}</time></dd>
        <dt>Motivo</dt><dd><blockquote>{{.Request.Reason}}</blockquote></dd>
      </dl>
    </article>

    <article class="master-grant-request-detail__column master-grant-request-detail__column--reviewer"
             aria-labelledby="grant-request-reviewer-title">
      <span class="master-grant-request-detail__sigil">{{icon "check"}} REVISOR (você)</span>
      <h2 id="grant-request-reviewer-title">O que acontece se aprovar</h2>
      <dl>
        <dt>Equivalência em tokens</dt>
        <dd>{{int64ToStr .Request.CapEquivalence}} tokens</dd>
        <dt>Estado atual</dt>
        <dd>{{grantRequestState .Request.State}}</dd>
        {{if not (isAwaiting .Request.State)}}
        <dt>Decidida em</dt>
        <dd><time datetime="{{.Request.DecidedAt.Format "2006-01-02T15:04:05Z07:00"}}">{{formatGrantTime .Request.DecidedAt}}</time></dd>
        <dt>Aprovador/Rejeitador</dt>
        <dd>{{.Request.SecondApproverID}}</dd>
        {{end}}
      </dl>

      {{if isAwaiting .Request.State}}
        {{if eqUUID .Request.CreatedByID $.CurrentUserID}}
          <p class="master-grant-request-detail__self-guard"
             role="alert"
             data-self-approve-guard="true">
            {{icon "octagon-alert"}} Você é o solicitante — não pode aprovar nem rejeitar a
            própria solicitação. Aguarde a revisão por outro master.
            (Regra 4-eyes — defendida em UI e backend.)
          </p>
        {{else if eq .ConfirmStage "confirm"}}
          <section class="master-grant-request-detail__confirm-modal"
                   role="dialog"
                   aria-modal="false"
                   aria-labelledby="confirm-modal-title"
                   data-confirm-modal="true">
            <h3 id="confirm-modal-title"
                class="master-grant-request-detail__confirm-title">
              Confirmar aprovação
            </h3>
            <p>
              Você está aprovando um grant <strong>acima do cap</strong>.
              Esta ação é registrada e auditada.
            </p>
            <dl>
              <dt>Tenant</dt><dd>{{.Request.TenantID}}</dd>
              <dt>Tipo</dt><dd>{{grantKindLabel .Request.Kind}}</dd>
              <dt>Valor</dt>
              <dd>
                {{if isFreePeriod (printf "%s" .Request.Kind)}}{{.Request.PeriodDays}} dias
                {{else if isExtraTokens (printf "%s" .Request.Kind)}}{{int64ToStr .Request.Amount}} tokens
                {{else}}—{{end}}
              </dd>
              <dt>Equivalência</dt>
              <dd>{{int64ToStr .Request.CapEquivalence}} tokens</dd>
              <dt>Solicitante</dt>
              <dd>{{.Request.CreatedByID}}</dd>
            </dl>
            <div class="master-grant-request-detail__confirm-actions">
              <form method="get"
                    action="/master/grants/requests/{{.Request.ID}}"
                    aria-label="Cancelar aprovação">
                <button type="submit"
                        class="master-grant-request-detail__confirm-cancel"
                        autofocus>Cancelar</button>
              </form>
              <form method="post"
                    action="/master/grants/requests/{{.Request.ID}}/approve"
                    hx-post="/master/grants/requests/{{.Request.ID}}/approve"
                    hx-target="#grant-request-detail"
                    hx-swap="outerHTML"
                    aria-label="Confirmar aprovação">
                {{.CSRFInput}}
                <input type="hidden" name="confirm" value="yes">
                <button type="submit"
                        class="master-grant-request-detail__confirm-final"
                        data-confirm-final="true">
                  CONFIRMAR APROVAÇÃO
                </button>
              </form>
            </div>
          </section>
        {{else}}
          <div class="master-grant-request-detail__actions">
            <form class="master-grant-request-detail__approve"
                  method="post"
                  action="/master/grants/requests/{{.Request.ID}}/approve"
                  hx-post="/master/grants/requests/{{.Request.ID}}/approve"
                  hx-target="#grant-request-detail"
                  hx-swap="outerHTML"
                  aria-label="Aprovar solicitação">
              {{.CSRFInput}}
              <button type="submit"
                      class="master-grant-request-detail__approve-submit"
                      data-approve-trigger="true">Aprovar…</button>
            </form>
            <form class="master-grant-request-detail__reject"
                  method="post"
                  action="/master/grants/requests/{{.Request.ID}}/reject"
                  hx-post="/master/grants/requests/{{.Request.ID}}/reject"
                  hx-target="#grant-request-detail"
                  hx-swap="outerHTML"
                  aria-label="Rejeitar solicitação"
                  hx-confirm="Confirmar rejeição da solicitação?">
              {{.CSRFInput}}
              <button type="submit"
                      class="master-grant-request-detail__reject-submit">Rejeitar</button>
            </form>
          </div>
        {{end}}
      {{else}}
        <p class="master-grant-request-detail__decided">
          Esta solicitação já foi {{grantRequestState .Request.State}}.
        </p>
      {{end}}
    </article>
  </div>
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
	registerImpersonationBanner(grantRequestsLayoutTmpl, grantRequestDetailLayoutTmpl)
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
