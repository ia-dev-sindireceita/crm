package dashboard

import (
	"html/template"
	"time"
)

// templateFuncs are the small formatting helpers the dashboard template
// uses. Keeping the formatting here (rather than baking pre-rendered
// strings into the view model) keeps the template declarative and the
// handler thin.
var templateFuncs = template.FuncMap{
	"stateLabel": stateLabel,
	"stateTone":  stateTone,
	"duration":   durationLabel,
	"channelLabel": func(c string) string {
		if c == "" {
			return "Desconhecido"
		}
		return c
	},
}

// stateLabel maps the raw lifecycle column value onto the pt-BR label the
// dashboard renders. Unknown values pass through unchanged so a new state
// added on the write path still surfaces (degraded but visible) rather
// than vanishing.
func stateLabel(state string) string {
	switch state {
	case "open":
		return "Abertas"
	case "closed":
		return "Fechadas"
	default:
		return state
	}
}

// stateTone maps the raw lifecycle column value onto the canonical
// Peitho StatusBadge tone modifier (`.badge--*`). Open conversations
// read as the active/accent tone; closed read neutral. Unknown states
// fall back to neutral so a newly added state still renders a valid
// pill rather than an unstyled chip.
func stateTone(state string) string {
	switch state {
	case "open":
		return "accent"
	case "closed":
		return "neutral"
	default:
		return "neutral"
	}
}

// durationLabel renders a duration for the HTML view. A zero duration
// means the underlying sample was empty (no replies / no closed
// conversations in the window), so it renders an em dash rather than a
// misleading "0s". Non-zero durations round to the second.
func durationLabel(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	return d.Round(time.Second).String()
}

// dashboardLayoutTmpl is the full dashboard page. It is a self-contained
// document (mirroring the inbox layout precedent) so SIN-65008 does not
// introduce a build pipeline. The per-tenant theme is applied via a
// nonce'd <style> block (the only inline style the strict CSP allows);
// every interactive affordance is a plain GET link, so there are no
// inline on*= handlers for the CSP to silently strip.
var dashboardLayoutTmpl = template.Must(template.New("dashboard.layout").Funcs(templateFuncs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Painel / relatórios</title>
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/tokens.css">
  <link rel="stylesheet" href="/static/css/components.css">
  <link rel="stylesheet" href="/static/css/dashboard.css">
</head>
<body>
  <main class="dashboard" role="main" data-testid="dashboard">
    <header class="dashboard__header">
      <h1>Painel / relatórios</h1>
      <p class="dashboard__period">Período: últimos 30 dias (desde {{.Since}}).</p>
      <p class="dashboard__actions">
        <a class="btn btn--secondary" href="/dashboard/export.csv" download data-testid="dashboard-export-csv">Exportar CSV (volume por canal)</a>
      </p>
    </header>

    <div class="dashboard__grid">
    <section class="dashboard__section card" aria-labelledby="dash-counters" data-testid="dashboard-counters">
      <h2 id="dash-counters">Conversas</h2>
      {{- if .HasStates}}
      <table class="dashboard__table">
        <thead><tr><th scope="col">Estado</th><th scope="col">Total</th></tr></thead>
        <tbody>
        {{- range .ConversationsByState}}
          <tr><td><span class="status-badge--peitho badge--{{stateTone .State}}">{{stateLabel .State}}</span></td><td>{{.Count}}</td></tr>
        {{- end}}
        </tbody>
      </table>
      {{- else}}
      <p class="dashboard__empty">Sem conversas no período.</p>
      {{- end}}
    </section>

    <section class="dashboard__section card" aria-labelledby="dash-sla" data-testid="dashboard-sla">
      <h2 id="dash-sla">Tempos de atendimento</h2>
      <table class="dashboard__table">
        <thead><tr><th scope="col">Indicador</th><th scope="col">p50</th><th scope="col">p90</th></tr></thead>
        <tbody>
          <tr>
            <td>Primeira resposta</td>
            <td>{{duration .FirstResponse.P50}}</td>
            <td>{{duration .FirstResponse.P90}}</td>
          </tr>
          <tr>
            <td>Resolução <small>(proxy — aproximação)</small></td>
            <td>{{duration .Resolution.P50}}</td>
            <td>{{duration .Resolution.P90}}</td>
          </tr>
        </tbody>
      </table>
    </section>

    <section class="dashboard__section card" aria-labelledby="dash-channels" data-testid="dashboard-channels">
      <h2 id="dash-channels">Volume por canal</h2>
      {{- if .HasChannels}}
      <table class="dashboard__table">
        <thead><tr><th scope="col">Canal</th><th scope="col">Conversas</th></tr></thead>
        <tbody>
        {{- range .VolumeByChannel}}
          <tr><td>{{channelLabel .Channel}}</td><td>{{.Count}}</td></tr>
        {{- end}}
        </tbody>
      </table>
      {{- else}}
      <p class="dashboard__empty">Sem volume por canal no período.</p>
      {{- end}}
    </section>

    <section class="dashboard__section card" aria-labelledby="dash-funnel" data-testid="dashboard-funnel">
      <h2 id="dash-funnel">Conversões por estágio do funil</h2>
      {{- if .HasFunnel}}
      <table class="dashboard__table">
        <thead><tr><th scope="col">Estágio</th><th scope="col">Conversas</th></tr></thead>
        <tbody>
        {{- range .FunnelByStage}}
          <tr><td>{{.Label}}</td><td>{{.Count}}</td></tr>
        {{- end}}
        </tbody>
      </table>
      {{- else}}
      <p class="dashboard__empty">Sem estágios de funil configurados.</p>
      {{- end}}
    </section>
    </div>
  </main>
</body>
</html>
`))
