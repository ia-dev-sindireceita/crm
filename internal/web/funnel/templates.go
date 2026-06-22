// Package funnel is the HTMX board UI for the basic sales funnel
// (SIN-62797, Fase 2 F2-12). Server-renders the five stage columns,
// posts drag-and-drop moves through FunnelService.MoveConversation
// (SIN-62792), and serves a per-conversation history modal that
// merges funnel transitions with the inbox assignment_history ledger.
//
// SIN-63943 / UX-F6 — board page now composes via shell.Layout (F1)
// for the post-login chrome (top-bar, branded nav, user-menu) and
// renders the analytical stats header (4 KPIs), per-stage column
// stats, and the RBAC-gated drawer. The legacy boardLayoutTmpl
// remains pointed at the shell-composed "layout" subtree so the
// templates_csp_nonce_test / templates_theme_test pinned assertions
// (which Execute the variable directly) still match.
//
// The package follows the same pattern as internal/web/inbox
// (SIN-62735): html/template, no JS framework, partial swaps via
// hx-* attributes. The minimal drag-and-drop + keyboard handler bit
// lives in /static/js/funnel-board.js — vanilla pointer events +
// HTMX trigger, CSP-safe (no inline JS).
package funnel

import (
	"embed"
	"html/template"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/pericles-luz/crm/internal/funnel"
	"github.com/pericles-luz/crm/internal/web/icon"
	"github.com/pericles-luz/crm/internal/web/shell"
)

//go:embed board_page.html stats_header.html stats_drawer.html
var contentFS embed.FS

var funcs = template.FuncMap{
	"relativeTime": relativeTime,
	"truncate":     truncate,
	"stageIcon":    stageIcon,
	"stageLabel":   stageLabel,
	"stageTone":    stageTone,
	"itoa":         itoa,
	"mulf":         func(a, b float64) float64 { return a * b },
	"durFmt":       durFmt,
	"derefF64": func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	},
	// Reflection-based accessors so the migrated board page template
	// stays renderable from the templates_csp_nonce_test +
	// templates_theme_test fixtures whose view struct predates the
	// SIN-63943 fields (Stats, CanSeeStats, CanSeeTeams, Filters,
	// Columns). Each helper returns the zero value when the field is
	// absent, so the {{range}} / {{if}} guards in the templates produce
	// no output rather than panicking.
	"funnelColumns":     funnelViewColumns,
	"funnelStats":       funnelViewStats,
	"funnelCanSeeStats": funnelViewBool("CanSeeStats"),
	"funnelCanSeeTeams": funnelViewBool("CanSeeTeams"),
	"funnelFilterValue": funnelViewFilterValue,
	"funnelCSRFToken":   funnelViewString("CSRFToken"),
}

// stageIcon returns the inline Lucide SVG for a stage column header
// (SIN-65098: replaces the legacy emoji glyph — "no emoji in chrome").
// The icon is purely decorative (rendered inside an aria-hidden span);
// the column's aria-label carries the stable, machine-readable name.
// Unknown keys fall back to a neutral circle.
func stageIcon(key string) template.HTML {
	name := "circle"
	switch key {
	case "novo":
		name = "sparkles"
	case "qualificando":
		name = "search"
	case "proposta":
		name = "edit"
	case "ganho":
		name = "check-circle"
	case "perdido":
		name = "x"
	}
	return icon.SVG(name, 16)
}

// stageLabel maps a stage key to its PT-BR sentence-case display label,
// used by the per-card StatusBadge. Unknown keys echo the raw key.
func stageLabel(key string) string {
	switch key {
	case "novo":
		return "Novo"
	case "qualificando":
		return "Qualificando"
	case "proposta":
		return "Proposta"
	case "ganho":
		return "Ganho"
	case "perdido":
		return "Perdido"
	default:
		return key
	}
}

// stageTone maps a stage key to a `.badge--*` modifier so the per-card
// StatusBadge picks up the canonical deal-stage tones from
// components.css (won/lost/nego/accent/neutral).
func stageTone(key string) string {
	switch key {
	case "ganho":
		return "won"
	case "perdido":
		return "lost"
	case "proposta":
		return "accent"
	case "qualificando":
		return "nego"
	default:
		return "neutral"
	}
}

func relativeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "agora"
	case d < time.Hour:
		return itoa(int(d/time.Minute)) + "min"
	case d < 24*time.Hour:
		return itoa(int(d/time.Hour)) + "h"
	default:
		return itoa(int(d/(24*time.Hour))) + "d"
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// funnelViewUnwrap unwraps a data value down to a struct value (or
// returns ok=false). Mirrors the shell package's pattern so missing
// fields never panic at template render.
func funnelViewUnwrap(data any) (reflect.Value, bool) {
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

// funnelViewBool builds a reflection-based bool getter for the given
// field name on the top-level view struct.
func funnelViewBool(field string) func(any) bool {
	return func(data any) bool {
		v, ok := funnelViewUnwrap(data)
		if !ok {
			return false
		}
		f := v.FieldByName(field)
		if !f.IsValid() {
			return false
		}
		if b, ok := f.Interface().(bool); ok {
			return b
		}
		return false
	}
}

// funnelViewString builds a reflection-based string getter for the
// given field name on the top-level view struct.
func funnelViewString(field string) func(any) string {
	return func(data any) string {
		v, ok := funnelViewUnwrap(data)
		if !ok {
			return ""
		}
		f := v.FieldByName(field)
		if !f.IsValid() {
			return ""
		}
		if s, ok := f.Interface().(string); ok {
			return s
		}
		return ""
	}
}

// funnelViewColumns returns the column slice off the view, or nil when
// the field is absent. Templates use {{range funnelColumns .}} to
// avoid html/template's strict "field not found" error.
func funnelViewColumns(data any) []columnView {
	v, ok := funnelViewUnwrap(data)
	if !ok {
		return nil
	}
	f := v.FieldByName("Columns")
	if !f.IsValid() {
		return nil
	}
	if s, ok := f.Interface().([]columnView); ok {
		return s
	}
	return nil
}

// funnelViewStats returns *funnel.Stats off the view, or nil when the
// field is absent or has the wrong type.
func funnelViewStats(data any) *funnel.Stats {
	v, ok := funnelViewUnwrap(data)
	if !ok {
		return nil
	}
	f := v.FieldByName("Stats")
	if !f.IsValid() {
		return nil
	}
	if s, ok := f.Interface().(*funnel.Stats); ok {
		return s
	}
	return nil
}

// funnelViewFilterValue returns one of the filter form's two string
// fields ("period", "owner") off the view's nested Filters struct.
// Returns "" when either layer is absent.
func funnelViewFilterValue(data any, field string) string {
	v, ok := funnelViewUnwrap(data)
	if !ok {
		return ""
	}
	f := v.FieldByName("Filters")
	if !f.IsValid() || f.Kind() != reflect.Struct {
		return ""
	}
	switch field {
	case "Period":
		ff := f.FieldByName("Period")
		if ff.IsValid() {
			if s, ok := ff.Interface().(string); ok {
				return s
			}
		}
	case "Owner":
		ff := f.FieldByName("Owner")
		if ff.IsValid() {
			if s, ok := ff.Interface().(string); ok {
				return s
			}
		}
	}
	return ""
}

// durFmt formats a duration as "Xd Yh" or "Zh" or "—" for zero.
func durFmt(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	if days > 0 {
		return itoa(days) + "d " + itoa(hours) + "h"
	}
	return itoa(hours) + "h"
}

// cardTmpl is the smallest swap unit: a single conversation card.
// POST /funnel/transitions targets the card via hx-target=outerHTML and
// re-renders this template with the new StageKey / PrevKey / NextKey so
// the keyboard buttons stay in sync with the card's new position.
var cardTmpl = template.Must(template.New("card").Funcs(funcs).Parse(`<li id="card-{{.ConversationID}}"
       class="funnel-card card"
       role="listitem"
       data-conversation-id="{{.ConversationID}}"
       data-stage-key="{{.StageKey}}"
       data-prev-key="{{.PrevKey}}"
       data-next-key="{{.NextKey}}"
       tabindex="0"
       aria-label="Conversa com {{.DisplayName}} ({{.Channel}}) em {{.StageKey}}">
    <header class="funnel-card__header">
      <span class="funnel-card__name">{{truncate .DisplayName 32}}</span>
      <span class="status-badge--pitho badge--{{stageTone .StageKey}} funnel-card__stage">{{stageLabel .StageKey}}</span>
    </header>
    <div class="funnel-card__meta">
      <span class="funnel-card__channel badge badge--neutral">{{.Channel}}</span>
      <time class="funnel-card__time" datetime="{{.LastMessageAt.Format "2006-01-02T15:04:05Z07:00"}}">{{relativeTime .LastMessageAt}}</time>
    </div>
    <nav class="funnel-card__actions" aria-label="Mover conversa">
{{- if .PrevKey}}
      <button type="button"
              class="funnel-card__action funnel-card__action--prev btn btn--sm"
              hx-post="/funnel/transitions"
              hx-vals='{"conversation_id":"{{.ConversationID}}","to_stage_key":"{{.PrevKey}}"}'
              hx-target="#card-{{.ConversationID}}"
              hx-swap="outerHTML"
              aria-label="Mover para {{.PrevKey}}">← {{.PrevKey}}</button>
{{- end}}
      <button type="button"
              class="funnel-card__action funnel-card__action--history btn btn--sm btn--ghost"
              hx-get="/funnel/conversations/{{.ConversationID}}/history"
              hx-target="#funnel-modal"
              hx-swap="innerHTML"
              aria-label="Histórico">histórico</button>
{{- if .NextKey}}
      <button type="button"
              class="funnel-card__action funnel-card__action--next btn btn--sm"
              hx-post="/funnel/transitions"
              hx-vals='{"conversation_id":"{{.ConversationID}}","to_stage_key":"{{.NextKey}}"}'
              hx-target="#card-{{.ConversationID}}"
              hx-swap="outerHTML"
              aria-label="Mover para {{.NextKey}}">{{.NextKey}} →</button>
{{- end}}
    </nav>
  </li>
`))

// historyModalTmpl renders the per-conversation history overlay. The
// timeline is the chronological merge of funnel transitions and
// assignment_history entries (oldest-first). The close button HTMX-
// swaps the empty mount back in.
var historyModalTmpl = template.Must(template.New("history").Funcs(funcs).Parse(`<div class="funnel-modal__backdrop" role="dialog" aria-modal="true" aria-labelledby="funnel-modal-title">
  <article class="funnel-modal__panel">
    <header class="funnel-modal__header">
      <h2 id="funnel-modal-title">Histórico</h2>
      <button type="button" class="funnel-modal__close"
              hx-get="/funnel/modal/close"
              hx-target="#funnel-modal"
              hx-swap="innerHTML"
              aria-label="Fechar histórico">×</button>
    </header>
    <ol class="funnel-modal__timeline" role="list">
    {{- range .Events}}
      <li class="funnel-modal__event funnel-modal__event--{{.Kind}}">
        <time class="funnel-modal__event-time" datetime="{{.At.Format "2006-01-02T15:04:05Z07:00"}}">{{.At.Format "02/01 15:04"}}</time>
        <span class="funnel-modal__event-text">{{.Text}}</span>
      </li>
    {{- else}}
      <li class="funnel-modal__event funnel-modal__event--empty">Sem histórico.</li>
    {{- end}}
    </ol>
  </article>
</div>
`))

// statsTmpl is the legacy HTMX partial for GET /funnel/stats (default
// view). Renders the header KPIs row, per-stage table, per-attendant
// table, and conditional per-team / per-channel tables. SIN-63962 / UX-F6
// shipped this; SIN-63943 keeps it intact for backwards compatibility with
// the existing handler-test pins and uses the new statsHeaderTmpl +
// statsDrawerTmpl partials for the F6 board-page integration.
var statsTmpl = template.Must(template.New("funnel.stats").Funcs(funcs).Parse(`<div class="funnel-stats" id="funnel-stats">
  <div class="funnel-stats__header">
    <span>Ativas: <strong>{{.Stats.HeaderKPIs.TotalActive}}</strong></span>
    <span>Ganhas: <strong>{{.Stats.HeaderKPIs.WonCount}}</strong></span>
    <span>Perdidas: <strong>{{.Stats.HeaderKPIs.LostCount}}</strong></span>
    <span>Taxa ganho: <strong>{{printf "%.1f%%" (mulf .Stats.HeaderKPIs.WonRate 100.0)}}</strong></span>
    <span>Tempo médio p/ ganho: <strong>{{durFmt .Stats.HeaderKPIs.AvgTimeToWin}}</strong></span>
  </div>
  {{if .Stats.Stages}}
  <table class="funnel-stats__stages">
    <caption>Por estágio</caption>
    <thead><tr><th>Estágio</th><th>Ativas</th><th>Tempo médio</th><th>Conv. rate</th></tr></thead>
    <tbody>
    {{range .Stats.Stages}}<tr>
      <td>{{.Label}}</td>
      <td>{{.ActiveCount}}</td>
      <td>{{durFmt .AvgTimeInStage}}</td>
      <td>{{if .ConvRate}}{{printf "%.1f%%" (mulf (derefF64 .ConvRate) 100.0)}}{{else}}—{{end}}</td>
    </tr>{{end}}
    </tbody>
  </table>
  {{end}}
  {{if .Stats.PerAttendant}}
  <table class="funnel-stats__attendants">
    <caption>Por atendente</caption>
    <thead><tr><th>ID</th><th>Ativas</th><th>Ganhas</th><th>Perdidas</th></tr></thead>
    <tbody>
    {{range .Stats.PerAttendant}}<tr>
      <td>{{.UserID}}</td>
      <td>{{.ActiveCount}}</td>
      <td>{{.WonCount}}</td>
      <td>{{.LostCount}}</td>
    </tr>{{end}}
    </tbody>
  </table>
  {{end}}
  {{if .Stats.PerTeam}}
  <table class="funnel-stats__teams">
    <caption>Por equipe</caption>
    <thead><tr><th>ID</th><th>Ativas</th><th>Ganhas</th><th>Perdidas</th></tr></thead>
    <tbody>
    {{range .Stats.PerTeam}}<tr>
      <td>{{.TeamID}}</td>
      <td>{{.ActiveCount}}</td>
      <td>{{.WonCount}}</td>
      <td>{{.LostCount}}</td>
    </tr>{{end}}
    </tbody>
  </table>
  {{end}}
  {{if .Stats.PerChannel}}
  <table class="funnel-stats__channels">
    <caption>Por canal</caption>
    <thead><tr><th>Canal</th><th>Ativas</th><th>Ganhas</th><th>Perdidas</th></tr></thead>
    <tbody>
    {{range .Stats.PerChannel}}<tr>
      <td>{{.Channel}}</td>
      <td>{{.ActiveCount}}</td>
      <td>{{.WonCount}}</td>
      <td>{{.LostCount}}</td>
    </tr>{{end}}
    </tbody>
  </table>
  {{end}}
</div>`))

// boardPageTmpl is the shell.Layout-composed full-page template used by
// the GET /funnel handler (SIN-63943 / UX-F6 / AC #8). It parses the
// content blocks ("title", "head_extra", "content") from board_page.html
// + the stats partials (funnel_stats_header, funnel_stats_drawer); the
// "card" sub-tree is grafted in by init() below.
var boardPageTmpl = shell.MustParse(funcs, contentFS,
	"board_page.html",
	"stats_header.html",
	"stats_drawer.html",
)

// boardLayoutTmpl is the entry point used by templates_csp_nonce_test
// and templates_theme_test, which call .Execute directly. We point it at
// the shell.Layout "layout" sub-tree so the existing assertions
// (`<style id="tenant-theme" nonce="…">…</style>` etc.) match against the
// migrated chrome. Both tests pass view structs that already expose
// CSPNonce + TenantThemeStyle, so shell.Layout's reflection-based
// helpers find the fields and render the tag.
var boardLayoutTmpl = boardPageTmpl.Lookup("layout")

// statsHeaderTmpl is the standalone partial for the page-header KPIs +
// filter form (SIN-63943 AC #1). The /funnel handler renders this
// inline; the filter form posts back to /funnel with hx-select on
// #funnel-board-area for the <400ms HTMX swap target.
var statsHeaderTmpl = boardPageTmpl.Lookup("funnel_stats_header")

// statsDrawerTmpl is the standalone partial for the drawer pane (AC #3).
// Returned by GET /funnel/stats?view=drawer; rendered into
// #funnel-stats-drawer by the "Estatísticas detalhadas" button.
var statsDrawerTmpl = boardPageTmpl.Lookup("funnel_stats_drawer")

func init() {
	// Graft cardTmpl + historyModalTmpl onto boardPageTmpl so the
	// content block can {{template "card" .}} / {{template "history" .}}.
	// The funnel_stats_header / funnel_stats_drawer partials are already
	// parsed via shell.MustParse above.
	for _, child := range []*template.Template{cardTmpl, historyModalTmpl} {
		if _, err := boardPageTmpl.AddParseTree(child.Name(), child.Tree); err != nil {
			panic("web/funnel: register " + child.Name() + ": " + err.Error())
		}
	}

	// Prime html/template's lazy escaper now, before any concurrent
	// goroutine can race on the first Execute call (same rationale as
	// web/inbox templates.go init prewarm — html/template AddParseTree
	// race fixed in bc30fb1).
	for _, t := range []*template.Template{cardTmpl, historyModalTmpl, boardPageTmpl, boardLayoutTmpl, statsTmpl, statsHeaderTmpl, statsDrawerTmpl} {
		if t != nil {
			_ = t.Execute(io.Discard, nil)
		}
	}
}
