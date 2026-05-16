// Package funnel is the HTMX board UI for the basic sales funnel
// (SIN-62797, Fase 2 F2-12). Server-renders the five stage columns,
// posts drag-and-drop moves through FunnelService.MoveConversation
// (SIN-62792), and serves a per-conversation history modal that
// merges funnel transitions with the inbox assignment_history ledger.
//
// The package follows the same pattern as internal/web/inbox
// (SIN-62735): html/template, no JS framework, partial swaps via
// hx-* attributes. The minimal drag-and-drop bit lives in
// /static/js/funnel-board.js — vanilla pointer events + HTMX trigger,
// CSP-safe (no inline JS).
package funnel

import (
	"html/template"
	"io"
	"strings"
	"time"
)

var funcs = template.FuncMap{
	"relativeTime": relativeTime,
	"truncate":     truncate,
	"stageGlyph":   stageGlyph,
}

// stageGlyph returns a small emoji glyph for the column header. The
// glyph keeps the column unambiguous when stage labels get long, but
// it is purely decorative — aria-label on the column carries the
// stable, machine-readable name.
func stageGlyph(key string) string {
	switch key {
	case "novo":
		return "🆕"
	case "qualificando":
		return "🔍"
	case "proposta":
		return "📝"
	case "ganho":
		return "🏆"
	case "perdido":
		return "💤"
	default:
		return "•"
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

// boardLayoutTmpl is the full-page shell. The board is a horizontal
// flex row of columns; the modal mount point is a sibling overlay
// HTMX swaps into on the history button click.
var boardLayoutTmpl = template.Must(template.New("funnel.layout").Funcs(funcs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Funil</title>
  {{.CSRFMeta}}
  <link rel="stylesheet" href="/static/css/funnel.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
  <script src="/static/js/funnel-board.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="funnel-shell" role="main" aria-label="Funil de vendas">
    <header class="funnel-shell__header"><h1>Funil</h1></header>
    <div id="funnel-board" class="funnel-board" data-csrf="{{.CSRFToken}}">
      {{template "board" .Board}}
    </div>
    <div id="funnel-modal" class="funnel-modal" aria-live="polite"></div>
  </main>
</body>
</html>
`))

// boardTmpl renders the five columns. Each column carries data-stage-key
// so funnel-board.js can resolve the drop destination, and each card
// inside it carries its own data-stage-key starting point.
var boardTmpl = template.Must(template.New("board").Funcs(funcs).Parse(`<section class="funnel-columns" role="region" aria-label="Estágios do funil">
{{- range .Columns}}
  <article class="funnel-column" aria-labelledby="col-{{.Stage.Key}}-title">
    <header class="funnel-column__header">
      <h2 id="col-{{.Stage.Key}}-title" class="funnel-column__title">
        <span class="funnel-column__glyph" aria-hidden="true">{{stageGlyph .Stage.Key}}</span>
        {{.Stage.Label}}
      </h2>
      <span class="funnel-column__count" aria-label="{{len .Cards}} conversas">{{len .Cards}}</span>
    </header>
    <ol class="funnel-column__list"
        role="list"
        data-stage-key="{{.Stage.Key}}"
        data-stage-id="{{.Stage.ID}}">
      {{- range .Cards}}
        {{template "card" .}}
      {{- end}}
    </ol>
  </article>
{{- end}}
</section>
`))

// cardTmpl is the smallest swap unit: a single conversation card.
// POST /funnel/transitions targets the card via hx-target=outerHTML and
// re-renders this template with the new StageKey / PrevKey / NextKey so
// the keyboard buttons stay in sync with the card's new position.
var cardTmpl = template.Must(template.New("card").Funcs(funcs).Parse(`<li id="card-{{.ConversationID}}"
       class="funnel-card"
       role="listitem"
       data-conversation-id="{{.ConversationID}}"
       data-stage-key="{{.StageKey}}"
       tabindex="0"
       aria-label="Conversa com {{.DisplayName}} ({{.Channel}}) em {{.StageKey}}">
    <header class="funnel-card__header">
      <span class="funnel-card__name">{{truncate .DisplayName 32}}</span>
      <span class="funnel-card__channel">{{.Channel}}</span>
    </header>
    <time class="funnel-card__time" datetime="{{.LastMessageAt.Format "2006-01-02T15:04:05Z07:00"}}">{{relativeTime .LastMessageAt}}</time>
    <nav class="funnel-card__actions" aria-label="Mover conversa">
{{- if .PrevKey}}
      <button type="button"
              class="funnel-card__action funnel-card__action--prev"
              hx-post="/funnel/transitions"
              hx-vals='{"conversation_id":"{{.ConversationID}}","to_stage_key":"{{.PrevKey}}"}'
              hx-target="#card-{{.ConversationID}}"
              hx-swap="outerHTML"
              aria-label="Mover para {{.PrevKey}}">← {{.PrevKey}}</button>
{{- end}}
      <button type="button"
              class="funnel-card__action funnel-card__action--history"
              hx-get="/funnel/conversations/{{.ConversationID}}/history"
              hx-target="#funnel-modal"
              hx-swap="innerHTML"
              aria-label="Histórico">histórico</button>
{{- if .NextKey}}
      <button type="button"
              class="funnel-card__action funnel-card__action--next"
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

func init() {
	// Register partials so the layout can reference {{template "board" ...}}
	// and the board can reference {{template "card" ...}}.
	for _, child := range []*template.Template{boardTmpl, cardTmpl, historyModalTmpl} {
		if _, err := boardLayoutTmpl.AddParseTree(child.Name(), child.Tree); err != nil {
			panic("web/funnel: register " + child.Name() + ": " + err.Error())
		}
	}
	if _, err := boardTmpl.AddParseTree(cardTmpl.Name(), cardTmpl.Tree); err != nil {
		panic("web/funnel: register card under board: " + err.Error())
	}

	// Prime html/template's lazy escaper now, before any concurrent
	// goroutine can race on the first Execute call (same rationale as
	// web/inbox templates.go init prewarm — html/template AddParseTree
	// race fixed in bc30fb1).
	for _, t := range []*template.Template{cardTmpl, boardTmpl, historyModalTmpl, boardLayoutTmpl} {
		_ = t.Execute(io.Discard, nil)
	}
}
