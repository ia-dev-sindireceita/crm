package dashboard

import (
	"html/template"
	"io"
	"time"

	"github.com/pericles-luz/crm/internal/metrics"
	"github.com/pericles-luz/crm/internal/web/shell"
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
	"maxCount": maxFunnelCount,
}

// maxFunnelCount returns the largest Count across all funnel stages,
// used as the <meter max="…"> upper bound so bars are relative to the
// leading stage. Returns 1 for an empty slice to avoid division by zero.
func maxFunnelCount(stages []metrics.StageCount) int64 {
	var m int64
	for _, s := range stages {
		if s.Count > m {
			m = s.Count
		}
	}
	if m == 0 {
		return 1
	}
	return m
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
// Pitho StatusBadge tone modifier (`.badge--*`). Open conversations
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

// dashboardLayoutTmpl is the full dashboard page. SIN-65122 migrates it
// onto the global SidebarNav app-shell (internal/web/shell) the way
// inbox/funnel did: the chrome (sidebar nav, brand, user menu, tenant
// theme, CSP nonce, impersonation banner) is owned by shell.Layout, and
// the dashboard's grid lives in the layout's "content" slot. The page's
// own stylesheet (dashboard.css) is injected via "head_extra"; tokens.css
// and components.css are already linked by the shell head. Every
// interactive affordance is a plain GET link, so there are no inline
// on*= handlers for the strict CSP to silently strip.
//
// It is exposed as the shell "layout" sub-tree so the handler can keep
// executing dashboardLayoutTmpl.Execute(w, data) against page data that
// carries the shell.Data chrome fields by name (the shell reflection
// helpers read them off the struct verbatim).
var dashboardLayoutTmpl = func() *template.Template {
	t := shell.MustParse(templateFuncs, nil)
	template.Must(t.Parse(`
{{define "title"}}Painel / relatórios{{end}}
{{define "head_extra"}}
  <link rel="stylesheet" href="/static/css/dashboard.css">
{{end}}
{{define "content"}}
  <div class="dashboard" data-testid="dashboard">

    <div class="dashboard__header">
      <div class="dashboard__header-text">
        <h1 class="dashboard__title">Painel / relatórios</h1>
        <p class="dashboard__period">Período: últimos 30 dias (desde {{.Since}}).</p>
      </div>
      <a class="btn btn--secondary" href="/dashboard/export.csv" download data-testid="dashboard-export-csv">Exportar CSV</a>
    </div>

    <div class="dashboard__metrics" role="list" aria-label="Indicadores do período">

      <div class="dashboard__metric-card card" role="listitem">
        <div class="dashboard__metric-top">
          <span class="dashboard__metric-icon" aria-hidden="true">{{icon "inbox" 16}}</span>
        </div>
        <div class="dashboard__metric-value tnum" aria-label="Conversas abertas">{{.OpenCount}}</div>
        <div class="dashboard__metric-label">Conversas abertas</div>
      </div>

      <div class="dashboard__metric-card card" role="listitem">
        <div class="dashboard__metric-top">
          <span class="dashboard__metric-icon" aria-hidden="true">{{icon "check-circle" 16}}</span>
        </div>
        <div class="dashboard__metric-value tnum" aria-label="Conversas fechadas">{{.ClosedCount}}</div>
        <div class="dashboard__metric-label">Conversas fechadas</div>
      </div>

      <div class="dashboard__metric-card card" role="listitem">
        <div class="dashboard__metric-top">
          <span class="dashboard__metric-icon" aria-hidden="true">{{icon "clock" 16}}</span>
        </div>
        <div class="dashboard__metric-value" aria-label="Tempo médio de primeira resposta">{{duration .FirstResponse.P50}}</div>
        <div class="dashboard__metric-label">Resp. média (p50)</div>
      </div>

      <div class="dashboard__metric-card card" role="listitem">
        <div class="dashboard__metric-top">
          <span class="dashboard__metric-icon" aria-hidden="true">{{icon "zap" 16}}</span>
        </div>
        <div class="dashboard__metric-value" aria-label="Tempo médio de resolução">{{duration .Resolution.P50}}</div>
        <div class="dashboard__metric-label">Resolução (p50)</div>
      </div>

    </div>

    <div class="dashboard__content">

      <section class="dashboard__panel card" aria-labelledby="dash-funnel" data-testid="dashboard-funnel">
        <h2 id="dash-funnel" class="dashboard__panel-title">Funil por estágio</h2>
        {{- if .HasFunnel}}
        {{- $max := maxCount .FunnelByStage}}
        <div class="dashboard__funnel-rows">
          {{- range .FunnelByStage}}
          <div class="dashboard__funnel-row">
            <div class="dashboard__funnel-meta">
              <span class="dashboard__funnel-name">{{.Label}}</span>
              <span class="dashboard__funnel-count tnum">{{.Count}}</span>
            </div>
            <meter class="dashboard__funnel-meter" value="{{.Count}}" min="0" max="{{$max}}"
              title="{{.Label}}: {{.Count}} conversas"></meter>
          </div>
          {{- end}}
        </div>
        {{- else}}
        <p class="dashboard__empty">Sem estágios de funil configurados.</p>
        {{- end}}
      </section>

      <div class="dashboard__side">

        <section class="dashboard__panel card" aria-labelledby="dash-counters" data-testid="dashboard-counters">
          <h2 id="dash-counters" class="dashboard__panel-title">Conversas</h2>
          {{- if .HasStates}}
          <table class="dashboard__table">
            <thead><tr><th scope="col">Estado</th><th scope="col">Total</th></tr></thead>
            <tbody>
            {{- range .ConversationsByState}}
              <tr><td><span class="status-badge--pitho badge--{{stateTone .State}}">{{stateLabel .State}}</span></td><td>{{.Count}}</td></tr>
            {{- end}}
            </tbody>
          </table>
          {{- else}}
          <p class="dashboard__empty">Sem conversas no período.</p>
          {{- end}}
        </section>

        <section class="dashboard__panel card" aria-labelledby="dash-channels" data-testid="dashboard-channels">
          <h2 id="dash-channels" class="dashboard__panel-title">Volume por canal</h2>
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

        <section class="dashboard__panel card" aria-labelledby="dash-sla" data-testid="dashboard-sla">
          <h2 id="dash-sla" class="dashboard__panel-title">Tempos de atendimento</h2>
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

      </div>
    </div>
  </div>
{{end}}
`))
	// Prime html/template's lazy escaper before any concurrent request can
	// race on the first Execute (mirrors the inbox/funnel precedent).
	_ = t.Execute(io.Discard, nil)
	return t.Lookup("layout")
}()
