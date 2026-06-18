package dashboard

import (
	"html/template"
	"io"
	"time"

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
  </div>
{{end}}
`))
	// Prime html/template's lazy escaper before any concurrent request can
	// race on the first Execute (mirrors the inbox/funnel precedent).
	_ = t.Execute(io.Discard, nil)
	return t.Lookup("layout")
}()
