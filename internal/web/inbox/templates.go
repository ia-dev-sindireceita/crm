// Package inbox is the HTMX inbox UI for SIN-62735 (Fase 1 PR9). The
// package owns three handlers — list, conversation view, and outbound
// send — and the html/template templates they render. It depends only
// on inbox use cases (internal/inbox/usecase), the tenancy context
// helper, and the CSRF helpers; the forbidwebboundary lint enforces
// that no handler reaches into the inbox domain root directly.
//
// Templating uses html/template from the stdlib so PR9 does not
// introduce a build pipeline. ADR-0073 §D1 + the existing
// internal/http/handler/aipanel package establish the precedent.
//
// SIN-63939 introduces a third (customer) pane on the right side of
// the inbox shell. The customer pane carries the contact summary, the
// IA-generated conversation digest + tips, and the per-conversation
// actions (transfer/close/funnel). The view handler renders both the
// middle (conversation) pane and the right (customer) pane in a single
// response — HTMX swaps the middle pane via hx-target, then applies
// the customer pane as an OOB swap so a conversation click refreshes
// both panes in one round-trip.
package inbox

import (
	"html/template"
	"io"
	"strings"
	"time"
)

// templateFuncs are the small set of helpers the templates use to render
// timestamps, badge classes, and direction-dependent CSS classes. Keeping
// them as funcs (rather than computing inside the handler) means the
// template stays declarative.
var templateFuncs = template.FuncMap{
	"relativeTime":     relativeTime,
	"relativeTimeLong": relativeTimeLong,
	"messageClass":     messageClass,
	"truncate":         truncate,
	"isFinalStatus":    isFinalStatus,
	"statusGlyph":      statusGlyph,
	"statusLabel":      statusLabel,
	"channelLabel":     channelLabel,
	"avatarInitial":    avatarInitial,
	"initials":         initials,
}

// finalStatuses are the terminal lifecycle states for outbound messages
// (SIN-62736 / ADR 0095). The realtime polling loop on the message
// bubble template renders WITHOUT hx-trigger attributes when the
// rendered status is final, so HTMX stops polling after the swap.
var finalStatuses = map[string]struct{}{
	"read":   {},
	"failed": {},
}

// isFinalStatus reports whether status terminates the outbound polling
// loop. The two final states are "read" (recipient opened the message)
// and "failed" (carrier rejected it). Pending / sent / delivered keep
// polling.
func isFinalStatus(status string) bool {
	_, ok := finalStatuses[status]
	return ok
}

// statusGlyph maps an outbound message status onto a WhatsApp-style
// indicator glyph. Inbound messages return the empty string — the
// status badge is conceptually about outbound delivery acks. Unknown
// statuses also return empty so the bubble degrades to "no badge"
// rather than dumping a raw label into the DOM.
func statusGlyph(status string) string {
	switch status {
	case "pending":
		return "⏱"
	case "sent":
		return "✓"
	case "delivered":
		return "✓✓"
	case "read":
		return "✓✓"
	case "failed":
		return "⚠"
	default:
		return ""
	}
}

// statusLabel is the screen-reader / aria text for the status badge.
// Server-rendered Portuguese so non-sighted users get the same context
// the glyph carries.
func statusLabel(status string) string {
	switch status {
	case "pending":
		return "Aguardando envio"
	case "sent":
		return "Enviada"
	case "delivered":
		return "Entregue"
	case "read":
		return "Lida"
	case "failed":
		return "Falha ao enviar"
	default:
		return ""
	}
}

// channelLabel returns the Portuguese display string for a channel
// identifier. Unknown channels are returned title-cased so the UI shows
// a sensible fallback (rather than dumping the raw key onto the canvas).
func channelLabel(channel string) string {
	switch strings.ToLower(channel) {
	case "whatsapp":
		return "WhatsApp"
	case "instagram":
		return "Instagram"
	case "messenger":
		return "Messenger"
	case "webchat":
		return "Webchat"
	case "facebook":
		return "Facebook"
	case "chatbot":
		return "Chatbot"
	case "":
		return "Canal desconhecido"
	default:
		return strings.ToUpper(channel[:1]) + channel[1:]
	}
}

// avatarInitial picks the first letter of a display name for the
// circular avatar placeholder. Returns "?" for empty / whitespace
// input so the avatar always renders a glyph.
func avatarInitial(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "?"
	}
	r := []rune(name)
	return strings.ToUpper(string(r[0]))
}

// initials derives a 1–2 letter monogram from a display name for the
// assignee chip (SIN-64968 §2.4). It takes the first letter of the first
// word and the first letter of the last word; a single-word name yields
// one letter. Returns "" for empty / whitespace input so the template can
// branch on it (the unassigned chip renders a dash instead).
func initials(name string) string {
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return ""
	}
	first := []rune(fields[0])
	out := strings.ToUpper(string(first[0]))
	if len(fields) > 1 {
		last := []rune(fields[len(fields)-1])
		out += strings.ToUpper(string(last[0]))
	}
	return out
}

// relativeTimeLong renders the spelled-out relative timestamp used by the
// row's <time aria-label> (SIN-64968 §5): "3" is too terse for a screen
// reader, so the accessible name carries "3 minutos" / "2 horas" / "agora
// mesmo". Returns "" for the zero time so the caller can omit the
// aria-label entirely.
func relativeTimeLong(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "agora mesmo"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minuto", "minutos")
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hora", "horas")
	default:
		return plural(int(d/(24*time.Hour)), "dia", "dias")
	}
}

// plural renders "<n> <one|many>" picking the singular form for n == 1.
func plural(n int, one, many string) string {
	if n <= 1 {
		return itoa(maxInt(n, 1)) + " " + one
	}
	return itoa(n) + " " + many
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// relativeTime renders a coarse "X seconds/minutes/hours/days ago" stamp
// for the conversation list. Server-rendered (no JS) so the inbox
// degrades cleanly when HTMX has not loaded yet.
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "agora"
	case d < time.Hour:
		return formatUnit(int(d/time.Minute), "min")
	case d < 24*time.Hour:
		return formatUnit(int(d/time.Hour), "h")
	default:
		return formatUnit(int(d/(24*time.Hour)), "d")
	}
}

func formatUnit(n int, unit string) string {
	if n <= 0 {
		return "agora"
	}
	return itoa(n) + unit
}

func itoa(n int) string {
	if n == 0 {
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

// messageClass maps the message direction to the CSS class the bubble
// uses. The template selects "msg-in"/"msg-out" so styling can live in
// /static/css/inbox.css alongside the other bundled stylesheets.
func messageClass(direction string) string {
	switch direction {
	case "out":
		return "msg-out"
	default:
		return "msg-in"
	}
}

// truncate shortens body for the list snippet. 80 chars + ellipsis is
// enough context for a triage glance without forcing a row reflow.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// inboxLayoutTmpl is the full-page shell. Three panes side-by-side: the
// left lists conversations, the middle renders the selected
// conversation (or an empty state), and the right (customer) pane
// surfaces contact + IA-summary + tips + actions for the active
// conversation. The CSRF token is rendered into both <meta> (for HTMX)
// and the outbound form's hidden field.
var inboxLayoutTmpl = template.Must(template.New("inbox.layout").Funcs(templateFuncs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Inbox</title>
  {{.CSRFMeta}}
  <meta name="htmx-config" content='{"includeIndicatorStyles":false}'>
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/tokens.css">
  <link rel="stylesheet" href="/static/css/inbox.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" nonce="{{$.CSPNonce}}" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="inbox-shell" role="main" data-testid="inbox-shell">
    <nav class="inbox-list-pane" aria-label="Conversas" data-testid="inbox-list-pane">
      {{template "inbox_list_region" .List}}
    </nav>
    <section id="inbox-conversation-pane" class="inbox-conversation-pane" aria-live="polite" aria-label="Conversa selecionada" data-testid="inbox-conversation-pane">
      <div class="inbox-empty" role="status">
        <p class="inbox-empty__title">Selecione uma conversa.</p>
        <p class="inbox-empty__hint" aria-hidden="true">← lista à esquerda</p>
      </div>
    </section>
    {{template "customer_panel" .Customer}}
    <button type="button" class="inbox-customer-pane__toggle" aria-controls="inbox-customer-pane" aria-expanded="false" data-testid="customer-pane-toggle">
      <span class="visually-hidden">Mostrar painel do cliente</span>
      <span aria-hidden="true">ⓘ</span>
    </button>
  </main>
  <script nonce="{{$.CSPNonce}}">
    // Single delegated listener for the AI-assist suggestion chips
    // (SIN-63977/65097). The chips are HTMX-swapped into #ai-assist-panel
    // and carry only data-suggestion — no inline hx-on/on* handler, which
    // the strict CSP would block. Clicking a chip copies its text into the
    // compose box and focuses it.
    document.addEventListener('click', function (e) {
      var btn = e.target.closest('.ai-assist__suggestion-btn');
      if (!btn) return;
      var body = document.getElementById('compose-body');
      if (!body) return;
      body.value = btn.getAttribute('data-suggestion') || '';
      body.focus();
    });
  </script>
</body>
</html>
`))

// inboxListRegionTmpl wraps the filter bar + the conversation list in a
// single swap target (SIN-64968). Both the filter controls (hx-target
// "#conversation-list-region", outerHTML) and the OOB row-active refresh
// from GET /inbox/conversations/{id} address THIS element by id, so the
// filter state (active pill / selected channel / "minhas" toggle) is
// re-rendered server-side alongside the list — no client-side state, no
// inline on* handlers (CSP of SIN-63977). When OOB is true the wrapper
// carries hx-swap-oob="true" so HTMX patches it in place from the
// conversation-view response without disturbing the conversation pane.
var inboxListRegionTmpl = template.Must(template.New("inbox_list_region").Funcs(templateFuncs).Parse(`<div id="conversation-list-region" class="inbox-list-region"{{if .OOB}} hx-swap-oob="true"{{end}}>
{{template "inbox_filters" .}}
{{template "conversation_list" .}}
</div>
`))

// inboxFiltersTmpl is the filter bar (SIN-64968 §3 + SIN-64979 §4). State
// is a group of pill links (hx-get); channel and the assignment queue are
// <select>s, all firing via hx-trigger="change" (an HTMX attribute — NOT
// an inline onchange handler, which the strict CSP would
// render-but-never-execute). The assignment queue is the SIN-64979 "visão
// de fila": "Todas" (no filter), "Não atribuídas" (unassigned queue), and
// "Atribuídas a mim" (the session user's conversations) — a single
// mutually-exclusive <select> rather than two toggles, matching the
// read-side use case which rejects the unassigned+user combination. Every
// control re-emits the full filter set (hx-include="closest form" + the
// pills' explicit hrefs) so combinations accumulate (AND) and the active
// state survives the swap. The whole region is the target so the active
// pill re-renders.
var inboxFiltersTmpl = template.Must(template.New("inbox_filters").Funcs(templateFuncs).Parse(`<form class="inbox-filters" role="search" aria-label="Filtrar conversas"
      hx-get="/inbox" hx-target="#conversation-list-region" hx-swap="outerHTML" hx-push-url="true">
  <div class="inbox-filters__group" role="group" aria-label="Estado">
    <a class="inbox-filters__pill{{if eq .Filters.State "open"}} is-active{{end}}"
       href="/inbox?state=open&channel={{.Filters.Channel}}&assigned={{.Filters.AssignedParam}}"
       {{if eq .Filters.State "open"}}aria-current="true"{{end}}
       hx-get="/inbox?state=open&channel={{.Filters.Channel}}&assigned={{.Filters.AssignedParam}}">Abertas</a>
    <a class="inbox-filters__pill{{if eq .Filters.State "closed"}} is-active{{end}}"
       href="/inbox?state=closed&channel={{.Filters.Channel}}&assigned={{.Filters.AssignedParam}}"
       {{if eq .Filters.State "closed"}}aria-current="true"{{end}}
       hx-get="/inbox?state=closed&channel={{.Filters.Channel}}&assigned={{.Filters.AssignedParam}}">Fechadas</a>
    <a class="inbox-filters__pill{{if eq .Filters.State ""}} is-active{{end}}"
       href="/inbox?state=&channel={{.Filters.Channel}}&assigned={{.Filters.AssignedParam}}"
       {{if eq .Filters.State ""}}aria-current="true"{{end}}
       hx-get="/inbox?state=&channel={{.Filters.Channel}}&assigned={{.Filters.AssignedParam}}">Todas</a>
  </div>
  <label class="inbox-filters__field">
    <span class="visually-hidden">Canal</span>
    <select name="channel" class="inbox-filters__select" hx-trigger="change" hx-get="/inbox" hx-include="closest form" hx-target="#conversation-list-region" hx-swap="outerHTML" hx-push-url="true">
      <option value=""{{if eq .Filters.Channel ""}} selected{{end}}>Todos os canais</option>
      <option value="whatsapp"{{if eq .Filters.Channel "whatsapp"}} selected{{end}}>WhatsApp</option>
      <option value="instagram"{{if eq .Filters.Channel "instagram"}} selected{{end}}>Instagram</option>
      <option value="messenger"{{if eq .Filters.Channel "messenger"}} selected{{end}}>Messenger</option>
      <option value="webchat"{{if eq .Filters.Channel "webchat"}} selected{{end}}>Webchat</option>
    </select>
  </label>
  <label class="inbox-filters__field">
    <span class="visually-hidden">Fila de atribuição</span>
    <select name="assigned" class="inbox-filters__select" aria-label="Fila de atribuição" hx-trigger="change" hx-get="/inbox" hx-include="closest form" hx-target="#conversation-list-region" hx-swap="outerHTML" hx-push-url="true">
      <option value=""{{if and (not .Filters.AssignedMe) (not .Filters.Unassigned)}} selected{{end}}>Todas as filas</option>
      <option value="unassigned"{{if .Filters.Unassigned}} selected{{end}}>Não atribuídas</option>
      <option value="me"{{if .Filters.AssignedMe}} selected{{end}}>Atribuídas a mim</option>
    </select>
  </label>
  <input type="hidden" name="state" value="{{.Filters.State}}">
</form>
`))

// conversationListTmpl is the conversation list itself (SIN-64968 §1–§2).
// Each row is a single <a> click target (Fitts) carrying the contact
// name, last-message snippet (with a "Você:" prefix when the last message
// was outbound), a relative timestamp, and the channel / awaiting-reply /
// closed / assignee badges. The row link carries the active filter query
// params ($.FilterQuery) so opening a conversation re-renders the OOB
// list under the same filters (CTO ratification of SIN-64966 §4.3,
// option a — no silent filter reset). The is-waiting / is-active
// modifiers add the left accent bar + bold name (redundant, non-colour
// signalling, WCAG 1.4.1).
//
// The hidden .conversation-list__channel span preserves the raw channel
// slug in the AOM (kept verbatim for the SIN-62919 regression test that
// scans for the literal class+text combination); the visible channel
// label is aria-hidden so the channel is not announced twice.
var conversationListTmpl = template.Must(template.New("conversation_list").Funcs(templateFuncs).Parse(`<ul class="conversation-list" id="conversation-list" role="list">
  {{range .Items}}
  <li class="conversation-list__item" role="listitem">
    <a href="/inbox/conversations/{{.ID}}{{$.FilterQuery}}"
       hx-get="/inbox/conversations/{{.ID}}{{$.FilterQuery}}"
       hx-target="#inbox-conversation-pane"
       hx-swap="innerHTML"
       hx-push-url="true"
       class="conversation-list__link{{if .AwaitingReply}} is-waiting{{end}}{{if .Active}} is-active{{end}}"
       {{if .Active}}aria-current="page"{{end}}>
      <span class="conversation-list__lead">
        {{if .AssigneeLabel}}
        <span class="assignee-chip" title="{{.AssigneeLabel}}"><span aria-hidden="true">{{.AssigneeInitials}}</span><span class="visually-hidden">Atribuída a {{.AssigneeLabel}}</span></span>
        {{else}}
        <span class="assignee-chip assignee-chip--unassigned"><span aria-hidden="true">—</span><span class="visually-hidden">Não atribuída</span></span>
        {{end}}
      </span>
      <span class="conversation-list__main">
        <span class="conversation-list__topline">
          <span class="conversation-list__name">{{if .ContactName}}{{.ContactName}}{{else}}Contato sem nome{{end}}</span>
          <time class="conversation-list__time" datetime="{{.LastMessageAt.Format "2006-01-02T15:04:05Z07:00"}}"{{if not .LastMessageAt.IsZero}} aria-label="Última mensagem há {{relativeTimeLong .LastMessageAt}}"{{end}}>{{relativeTime .LastMessageAt}}</time>
        </span>
        <span class="conversation-list__snippet">{{if .OutboundLast}}<span class="conversation-list__snippet-prefix">Você:</span> {{end}}{{truncate .Snippet 80}}</span>
        <span class="conversation-list__badges">
          <span class="conversation-list__channel-badge">{{template "channel_badge" .Channel}}<span class="conversation-list__channel-tag" aria-hidden="true">{{channelLabel .Channel}}</span></span>
          <span class="conversation-list__channel">{{.Channel}}</span>
          {{if .AwaitingReply}}<span class="waiting-badge"><span class="waiting-badge__dot" aria-hidden="true">●</span> Aguardando resposta</span>{{end}}
          {{if .Closed}}<span class="state-badge state-badge--closed">Fechada</span>{{end}}
        </span>
      </span>
    </a>
  </li>
  {{else}}
  {{if .HasFilters}}
  <li class="conversation-list__empty conversation-list__empty--filtered">
    <p class="conversation-list__empty-title">Nenhuma conversa com estes filtros.</p>
    <a class="conversation-list__empty-clear" hx-get="/inbox" hx-target="#conversation-list-region" hx-swap="outerHTML" hx-push-url="true" href="/inbox">Limpar filtros</a>
  </li>
  {{else}}
  <li class="conversation-list__empty">Nenhuma conversa. As novas conversas aparecem aqui assim que chegam pelos canais conectados.</li>
  {{end}}
  {{end}}
</ul>
`))

// channelBadgeTmpl renders a per-channel SVG icon plus a screen-reader-
// only label. The four supported channels (whatsapp / instagram /
// facebook / chatbot) carry brand-aligned glyphs; unknown channels
// degrade to a neutral dot so the badge grid never collapses.
//
// WCAG 1.4.1 (cor não-único diferenciador): every badge pairs the
// channel-coloured SVG with a screen-reader text label and a
// data-channel="…" attribute so styling never relies on colour alone.
var channelBadgeTmpl = template.Must(template.New("channel_badge").Funcs(templateFuncs).Parse(`<span class="channel-badge channel-badge--{{if .}}{{.}}{{else}}unknown{{end}}" data-channel="{{.}}" data-testid="channel-badge-{{if .}}{{.}}{{else}}unknown{{end}}" role="img" aria-label="Canal: {{channelLabel .}}">
{{- if eq . "whatsapp" -}}
<svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" focusable="false"><path fill="currentColor" d="M12 2C6.48 2 2 6.48 2 12c0 1.95.56 3.77 1.52 5.31L2 22l4.85-1.5A9.96 9.96 0 0 0 12 22c5.52 0 10-4.48 10-10S17.52 2 12 2zm5.2 14.31c-.22.6-1.27 1.13-1.74 1.2-.46.07-1.04.1-1.66-.1a13.43 13.43 0 0 1-5.99-5.13c-.47-.62-.78-1.39-.78-2.13 0-.84.42-1.27.6-1.45.18-.18.4-.22.53-.22h.38c.13 0 .3-.04.46.35.18.4.6 1.45.65 1.56.05.1.07.22.02.36-.05.13-.07.22-.16.34l-.27.31c-.09.1-.18.21-.08.4.1.18.46.76.99 1.23.68.6 1.25.78 1.43.87.18.09.28.07.39-.04.1-.11.45-.52.57-.7.11-.18.23-.15.38-.09.16.06 1.02.48 1.2.57.18.09.3.13.34.21.05.09.05.5-.17 1.1z"/></svg>
{{- else if eq . "instagram" -}}
<svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" focusable="false"><path fill="currentColor" d="M12 2.2c3.2 0 3.58.01 4.85.07 1.17.05 1.8.25 2.23.41.56.22.96.48 1.38.9.42.42.68.82.9 1.38.16.42.36 1.06.41 2.23.06 1.27.07 1.65.07 4.85s-.01 3.58-.07 4.85c-.05 1.17-.25 1.8-.41 2.23-.22.56-.48.96-.9 1.38-.42.42-.82.68-1.38.9-.42.16-1.06.36-2.23.41-1.27.06-1.65.07-4.85.07s-3.58-.01-4.85-.07c-1.17-.05-1.8-.25-2.23-.41-.56-.22-.96-.48-1.38-.9-.42-.42-.68-.82-.9-1.38-.16-.42-.36-1.06-.41-2.23C2.21 15.58 2.2 15.2 2.2 12s.01-3.58.07-4.85c.05-1.17.25-1.8.41-2.23.22-.56.48-.96.9-1.38.42-.42.82-.68 1.38-.9.42-.16 1.06-.36 2.23-.41C8.42 2.21 8.8 2.2 12 2.2zm0 1.8c-3.15 0-3.5.01-4.74.07-1 .04-1.55.21-1.91.35-.48.19-.83.41-1.19.77-.36.36-.58.71-.77 1.19-.14.36-.31.91-.35 1.91C3 8.5 2.99 8.85 2.99 12s.01 3.5.07 4.74c.04 1 .21 1.55.35 1.91.19.48.41.83.77 1.19.36.36.71.58 1.19.77.36.14.91.31 1.91.35 1.24.06 1.59.07 4.74.07s3.5-.01 4.74-.07c1-.04 1.55-.21 1.91-.35.48-.19.83-.41 1.19-.77.36-.36.58-.71.77-1.19.14-.36.31-.91.35-1.91.06-1.24.07-1.59.07-4.74s-.01-3.5-.07-4.74c-.04-1-.21-1.55-.35-1.91a3.2 3.2 0 0 0-.77-1.19 3.2 3.2 0 0 0-1.19-.77c-.36-.14-.91-.31-1.91-.35C15.5 4.01 15.15 4 12 4zm0 3.07a4.93 4.93 0 1 1 0 9.86 4.93 4.93 0 0 1 0-9.86zm0 1.8a3.13 3.13 0 1 0 0 6.26 3.13 3.13 0 0 0 0-6.26zm5.07-2.13a1.15 1.15 0 1 1 0 2.3 1.15 1.15 0 0 1 0-2.3z"/></svg>
{{- else if eq . "facebook" -}}
<svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" focusable="false"><path fill="currentColor" d="M22 12a10 10 0 1 0-11.56 9.88v-6.99H7.9V12h2.54V9.8c0-2.51 1.5-3.9 3.78-3.9 1.1 0 2.24.2 2.24.2v2.46h-1.26c-1.24 0-1.63.77-1.63 1.56V12h2.78l-.44 2.89h-2.34v6.99A10 10 0 0 0 22 12z"/></svg>
{{- else if eq . "chatbot" -}}
<svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" focusable="false"><path fill="currentColor" d="M12 2a2 2 0 0 0-2 2v1H7a3 3 0 0 0-3 3v8a3 3 0 0 0 3 3h10a3 3 0 0 0 3-3V8a3 3 0 0 0-3-3h-3V4a2 2 0 0 0-2-2zM8.5 11a1.5 1.5 0 1 1 0 3 1.5 1.5 0 0 1 0-3zm7 0a1.5 1.5 0 1 1 0 3 1.5 1.5 0 0 1 0-3zM3 11.5A1.5 1.5 0 0 0 1.5 13v2a1.5 1.5 0 0 0 3 0v-2A1.5 1.5 0 0 0 3 11.5zm18 0a1.5 1.5 0 0 0-1.5 1.5v2a1.5 1.5 0 0 0 3 0v-2A1.5 1.5 0 0 0 21 11.5z"/></svg>
{{- else -}}
<svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" focusable="false"><circle cx="12" cy="12" r="4" fill="currentColor"/></svg>
{{- end -}}
<span class="visually-hidden">{{channelLabel .}}</span></span>
`))

// conversationViewTmpl is the right-pane HTMX partial for a single
// conversation. The thread of message_bubble blocks is followed by the
// outbound form (CSRF + hidden conversation id + textarea).
//
// CustomerPanel carries the OOB-swap markup for the right (customer)
// pane — it lands in the same response so HTMX applies both the
// middle-pane swap (the conversation thread) and the customer-pane
// refresh (contact + IA summary/tips) in one round-trip.
var conversationViewTmpl = template.Must(template.New("conversation_view").Funcs(templateFuncs).Parse(`<article class="conversation" aria-label="Conversa">
  <header class="conversation__header">
    <h1 class="conversation__title">Conversa</h1>
    {{template "channel_badge" .Channel}}
  </header>
  {{template "conversation_context" .Context}}
  <ol id="conversation-thread" class="conversation__thread" role="list">
    {{range .Messages}}
      {{template "message_bubble" .}}
    {{end}}
  </ol>
  <form class="conversation__compose"
        hx-post="/inbox/conversations/{{.ConversationID}}/messages"
        hx-target="#conversation-thread"
        hx-swap="beforeend"
        hx-indicator="#compose-indicator">
    {{.CSRFInput}}
    <label for="compose-body" class="visually-hidden">Mensagem</label>
    {{template "compose_textarea" false}}
    <button type="submit" class="conversation__compose-submit">Enviar</button>
    <span id="compose-indicator" class="conversation__compose-indicator" role="status" aria-live="polite">Enviando…</span>
  </form>
</article>
{{.CustomerPanel}}
`))

// conversationContextTmpl is the conversation context side panel
// (SIN-64970, frontend half of SIN-64959). It surfaces the read-only
// ConversationContextView projection — contact identity, channel,
// funnel stage, and assignment state — beside the message thread so the
// operator has the sale context without leaving the conversation.
//
// Every block is partial-data tolerant: a missing contact renders
// "Contato sem nome" and no identity list; a conversation that has never
// moved on the funnel renders "Sem etapa definida"; an unassigned
// conversation renders "Não atribuída". When the whole context read was
// skipped or failed (HasContext=false) the panel collapses to a single
// "Contexto indisponível" line rather than a half-empty card.
//
// Accessibility: the panel is a semantic <aside> with a labelled region
// per facet (Contato / Canal / Funil / Atribuição). CSP-safe: no inline
// on*= handlers — the panel is pure server-rendered markup.
var conversationContextTmpl = template.Must(template.New("conversation_context").Funcs(templateFuncs).Parse(`<aside class="conversation-context" aria-labelledby="conversation-context-title" data-testid="conversation-context">
  <h2 id="conversation-context-title" class="conversation-context__title">Contexto da conversa</h2>
{{- if .HasContext}}
  <section class="conversation-context__section conversation-context__contact" aria-label="Contato" data-testid="conversation-context-contact">
    <h3 class="conversation-context__subtitle">Contato</h3>
    <p class="conversation-context__contact-name">{{if .ContactName}}{{.ContactName}}{{else}}Contato sem nome{{end}}</p>
    {{- if .Identities}}
    <ul class="conversation-context__identities" role="list">
      {{- range .Identities}}
      <li class="conversation-context__identity">
        {{template "channel_badge" .Channel}}
        <span class="conversation-context__identity-id">{{.ExternalID}}</span>
      </li>
      {{- end}}
    </ul>
    {{- end}}
  </section>
  <section class="conversation-context__section conversation-context__channel" aria-label="Canal" data-testid="conversation-context-channel">
    <h3 class="conversation-context__subtitle">Canal</h3>
    <p class="conversation-context__channel-value">
      {{template "channel_badge" .Channel}}
      <span class="conversation-context__channel-label">{{channelLabel .Channel}}</span>
    </p>
  </section>
  <section class="conversation-context__section conversation-context__funnel" aria-label="Etapa do funil" data-testid="conversation-context-funnel">
    <h3 class="conversation-context__subtitle">Etapa do funil</h3>
    {{- if .FunnelStageName}}
    <p class="conversation-context__funnel-stage" data-stage-key="{{.FunnelStageKey}}">{{.FunnelStageName}}</p>
    {{- else}}
    <p class="conversation-context__funnel-empty">Sem etapa definida</p>
    {{- end}}
  </section>
  {{template "conversation_assignment" .}}
{{- else}}
  <p class="conversation-context__empty" data-testid="conversation-context-empty">Contexto indisponível.</p>
{{- end}}
</aside>
`))

// conversationAssignmentTmpl renders the assignment section of the
// conversation context panel (SIN-64979). It is also the standalone
// HTMX swap target for POST /inbox/conversations/{id}/assign responses:
// the form targets "#conversation-context-assignment" with
// hx-swap="outerHTML", so the element returned here replaces the old
// section in place without reloading the full context panel.
//
// When .Assignees is non-nil the section renders an interactive
// dropdown + "Atribuir a mim" shortcut; when nil it degrades to the
// same read-only "Atribuída / Não atribuída" text the context panel
// showed before SIN-64979 so unwired deployments keep the same UX.
//
// CSP-safe: no inline on*= handlers — the forms use hx-* attributes
// exclusively. The parent layout's hx-headers body attribute propagates
// the X-CSRF-Token to all HTMX requests, so no hidden field is needed
// inside the partial.
var conversationAssignmentTmpl = template.Must(template.New("conversation_assignment").Funcs(templateFuncs).Parse(`<section id="conversation-context-assignment" class="conversation-context__section conversation-context__assignment" aria-label="Atribuição" data-testid="conversation-context-assignment">
  <h3 class="conversation-context__subtitle">Atribuição</h3>
{{- if .Assignees}}
  <p class="conversation-context__assignment-current">
    {{- if .AssignedDisplayName}}
    <span class="assignee-chip" title="{{.AssignedDisplayName}}"><span aria-hidden="true">{{initials .AssignedDisplayName}}</span></span>
    <span class="conversation-context__assignment-label">{{.AssignedDisplayName}}</span>
    {{- else if .Assigned}}
    <span class="assignee-chip assignee-chip--unknown"><span aria-hidden="true">?</span></span>
    <span class="conversation-context__assignment-label">Atribuída</span>
    {{- else}}
    <span class="assignee-chip assignee-chip--unassigned" aria-hidden="true">—</span>
    <span class="conversation-context__assignment-label conversation-context__assignment-label--unassigned">Não atribuída</span>
    {{- end}}
  </p>
  <form class="conversation-context__assign-form"
        hx-post="/inbox/conversations/{{.ConversationIDStr}}/assign"
        hx-target="#conversation-context-assignment"
        hx-swap="outerHTML">
    <label for="assign-target-{{.ConversationIDStr}}" class="visually-hidden">Atribuir a</label>
    <select id="assign-target-{{.ConversationIDStr}}" name="targetUserID" class="conversation-context__assign-select">
      {{- range .Assignees}}
      <option value="{{.UserID}}">{{.DisplayName}}</option>
      {{- end}}
    </select>
    <button type="submit" class="conversation-context__assign-btn">Atribuir</button>
  </form>
  {{- if .CurrentUserID}}
  <form class="conversation-context__assign-me-form"
        hx-post="/inbox/conversations/{{.ConversationIDStr}}/assign"
        hx-target="#conversation-context-assignment"
        hx-swap="outerHTML">
    <input type="hidden" name="targetUserID" value="{{.CurrentUserID}}">
    <button type="submit" class="conversation-context__assign-me-btn">Atribuir a mim</button>
  </form>
  {{- end}}
{{- else}}
  {{- if .Assigned}}
  <p class="conversation-context__assignment-value conversation-context__assignment-value--assigned">Atribuída</p>
  {{- else}}
  <p class="conversation-context__assignment-value conversation-context__assignment-value--unassigned">Não atribuída</p>
  {{- end}}
{{- end}}
</section>
`))

// customerPanelTmpl is the right rail. It is rendered both inside the
// layout (initial empty state) AND from the view handler with
// hx-swap-oob="outerHTML" so a conversation click refreshes the
// customer info, the LLM summary slot, the LLM tips slot, and the
// per-conversation actions in a single response.
//
// The IA-assist button + #ai-assist-panel placeholder live inside the
// "Resumo da conversa (IA)" block — clicking the button POSTs to
// /inbox/conversations/{id}/ai-assist and the response is swapped into
// the panel anchor. The summary, tips, and the "usar sugestão" wiring
// reuse the existing assist machinery (ai_assist.go) — this template
// just relocates the panel onto the right rail per the F2 spec.
var customerPanelTmpl = template.Must(template.New("customer_panel").Funcs(templateFuncs).Parse(`<aside id="inbox-customer-pane" class="inbox-customer-pane" aria-label="Informações do cliente" aria-live="polite" data-testid="customer-panel" hx-swap-oob="outerHTML">
{{- if .HasConversation}}
  <section class="customer-card" data-testid="customer-card">
    <div class="customer-card__avatar" aria-hidden="true">{{avatarInitial .Contact.DisplayName}}</div>
    <div class="customer-card__identity">
      <h2 class="customer-card__name">{{if .Contact.DisplayName}}{{.Contact.DisplayName}}{{else}}Contato sem nome{{end}}</h2>
      {{template "channel_badge" .Channel}}
    </div>
    {{- if or .Contact.Email .Contact.Phone .Contact.Tags}}
    <dl class="customer-card__meta">
      {{- if .Contact.Email}}
      <div class="customer-card__meta-row">
        <dt>Email</dt>
        <dd>{{.Contact.Email}}</dd>
      </div>
      {{- end}}
      {{- if .Contact.Phone}}
      <div class="customer-card__meta-row">
        <dt>Telefone</dt>
        <dd>{{.Contact.Phone}}</dd>
      </div>
      {{- end}}
      {{- if .Contact.Tags}}
      <div class="customer-card__meta-row">
        <dt>Tags</dt>
        <dd>
          <ul class="customer-card__tags" role="list">
            {{- range .Contact.Tags}}<li class="customer-card__tag">{{.}}</li>{{end}}
          </ul>
        </dd>
      </div>
      {{- end}}
    </dl>
    {{- end}}
  </section>

  {{- if .Contact.Identities}}
  <section class="customer-identities" aria-labelledby="customer-identities-title" data-testid="customer-identities">
    <h3 id="customer-identities-title" class="customer-section__title">Identidades vinculadas</h3>
    <ul class="customer-identities__list" role="list">
      {{- range .Contact.Identities}}
      <li class="customer-identities__item">
        {{template "channel_badge" .Channel}}
        <span class="customer-identities__handle">{{.Handle}}</span>
        <button type="button" class="customer-identities__split" disabled title="Separar identidades — em breve">Separar</button>
      </li>
      {{- end}}
    </ul>
  </section>
  {{- end}}

  <section class="customer-summary" aria-labelledby="customer-summary-title" data-testid="customer-summary">
    <header class="customer-section__header">
      <h3 id="customer-summary-title" class="customer-section__title">Resumo da conversa (IA)</h3>
    </header>
    {{- if .AssistButton}}
    {{.AssistButton}}
    <section id="ai-assist-panel" class="customer-summary__panel ai-assist__panel" data-testid="customer-summary-panel" aria-live="polite">
      <p class="customer-summary__empty">Clique para gerar um resumo e 3 dicas para fechar a venda.</p>
    </section>
    <section id="ai-consent-modal" class="ai-consent-modal" hidden></section>
    {{- else}}
    <p class="customer-summary__empty" data-testid="customer-summary-disabled">IA não está disponível neste tenant.</p>
    {{- end}}
  </section>

  <section class="customer-tips" aria-labelledby="customer-tips-title" data-testid="customer-tips">
    <h3 id="customer-tips-title" class="customer-section__title">Dicas para fechar venda (IA)</h3>
    <p class="customer-tips__hint">As sugestões aparecem dentro do resumo acima. Cada sugestão tem um botão "usar esta sugestão" que preenche a caixa de resposta.</p>
    <p class="customer-tips__policy"><a href="/settings/ai-policy">Ver política de IA</a></p>
  </section>

  <section class="customer-actions" aria-labelledby="customer-actions-title" data-testid="customer-actions">
    <h3 id="customer-actions-title" class="customer-section__title">Ações</h3>
    <ul class="customer-actions__list" role="list">
      <li><button type="button" class="customer-actions__btn" disabled title="Em breve">Transferir conversa</button></li>
      <li><button type="button" class="customer-actions__btn" disabled title="Em breve">Encerrar conversa</button></li>
      <li><a class="customer-actions__btn customer-actions__btn--link" href="/funnel?conversation={{.ConversationID}}">Ver no funil</a></li>
    </ul>
  </section>
{{- else}}
  <section class="customer-empty" data-testid="customer-empty">
    <h2 class="customer-empty__title">Selecione uma conversa</h2>
    <p class="customer-empty__hint">Os dados do cliente, o resumo da conversa e as sugestões de resposta aparecem aqui.</p>
  </section>
{{- end}}
</aside>
`))

// messageBubbleTmpl is the smallest swap unit: a single message bubble
// inside the conversation thread. POST /inbox/conversations/:id/messages
// renders this template into the thread with hx-swap=beforeend;
// GET /inbox/conversations/:id/messages/:msgID/status re-renders the
// same template with hx-swap=outerHTML during the realtime polling loop
// (SIN-62736 / ADR 0095). The hx-* polling attributes are emitted ONLY
// for outbound messages in a non-final status — once the status reaches
// "read" or "failed" the next swap drops the attributes and HTMX stops
// polling for this bubble.
var messageBubbleTmpl = template.Must(template.New("message_bubble").Funcs(templateFuncs).Parse(`<li id="msg-{{.ID}}" class="message-bubble {{messageClass .Direction}}" data-status="{{.Status}}" role="listitem"
{{- if and (eq .Direction "out") (not (isFinalStatus .Status))}}
  hx-get="/inbox/conversations/{{.ConversationID}}/messages/{{.ID}}/status?currentStatus={{.Status}}"
  hx-trigger="every 3s"
  hx-target="this"
  hx-swap="outerHTML"
{{- end}}>
  <p class="message-bubble__body">{{.Body}}</p>
  {{- if .Media}}
    {{- if eq .Media.ScanStatus "infected"}}
  <div class="message-bubble__media message-bubble__media--blocked" role="status" aria-label="Anexo bloqueado por segurança">
    <span class="message-bubble__media-icon" aria-hidden="true">⛔</span>
    <span class="message-bubble__media-blocked-text">Conteúdo bloqueado por segurança</span>
  </div>
    {{- else if eq .Media.ScanStatus "clean"}}
  <a class="message-bubble__media message-bubble__media--clean" href="/t/{{.Media.Hash}}/m" data-format="{{.Media.Format}}">
    <span class="message-bubble__media-icon" aria-hidden="true">📎</span>
    <span class="message-bubble__media-link-text">Anexo</span>
  </a>
    {{- else}}
  <div class="message-bubble__media message-bubble__media--pending" role="status" aria-label="Anexo aguardando verificação">
    <span class="message-bubble__media-icon" aria-hidden="true">…</span>
    <span class="message-bubble__media-pending-text">Verificando anexo</span>
  </div>
    {{- end}}
  {{- end}}
  <time class="message-bubble__time" datetime="{{.CreatedAt.Format "2006-01-02T15:04:05Z07:00"}}">{{relativeTime .CreatedAt}}</time>
  {{- if eq .Direction "out"}}
  {{- $glyph := statusGlyph .Status}}{{if $glyph}}
  <span class="message-bubble__status message-bubble__status--{{.Status}}" aria-label="{{statusLabel .Status}}" title="{{statusLabel .Status}}">{{$glyph}}</span>
  {{- end}}
  {{- end}}
</li>
`))

// composeTextareaTmpl is the single source of truth for the outbound
// compose <textarea>. It renders in two places that MUST stay identical:
//
//   - inside conversationViewTmpl's compose form ({{template
//     "compose_textarea" false}}), and
//   - as a standalone out-of-band fragment from the send handler
//     ({{template "compose_textarea" true}}), which clears the field
//     after a successful POST.
//
// The dot is a bool: true emits hx-swap-oob="true" so htmx replaces the
// live #compose-body element by id (a pure DOM swap — no new Function,
// no eval), false omits it for the in-form render. This replaces the old
// hx-on::after-request="this.reset()" which htmx compiled with
// new Function(...) and which therefore threw EvalError under the prod
// strict CSP (script-src without 'unsafe-eval') — see SIN-65067/SIN-65068.
var composeTextareaTmpl = template.Must(template.New("compose_textarea").Parse(
	`<textarea id="compose-body" name="body" rows="3" maxlength="4096" required` +
		` placeholder="Escreva sua resposta…"{{if .}} hx-swap-oob="true"{{end}}></textarea>`))

func init() {
	// Cross-register partials so the layout can render
	// {{template "conversation_list" …}} and so on with one template
	// tree. Errors here are programmer errors (typos in the template
	// source) — surface them at process start, not at request time.
	for _, child := range []*template.Template{inboxListRegionTmpl, inboxFiltersTmpl, conversationListTmpl, conversationViewTmpl, messageBubbleTmpl, customerPanelTmpl, channelBadgeTmpl, conversationContextTmpl, conversationAssignmentTmpl} {
		if _, err := inboxLayoutTmpl.AddParseTree(child.Name(), child.Tree); err != nil {
			panic("inbox/web: register " + child.Name() + ": " + err.Error())
		}
	}
	// inboxListRegionTmpl is executed standalone for HTMX partial swaps
	// (filter changes) and OOB refreshes (row activation), so it needs the
	// filter + list + badge partials registered on its own tree.
	for _, child := range []*template.Template{inboxFiltersTmpl, conversationListTmpl, channelBadgeTmpl} {
		if _, err := inboxListRegionTmpl.AddParseTree(child.Name(), child.Tree); err != nil {
			panic("inbox/web: register " + child.Name() + " in list region: " + err.Error())
		}
	}
	for _, child := range []*template.Template{messageBubbleTmpl, customerPanelTmpl, channelBadgeTmpl, conversationContextTmpl, conversationAssignmentTmpl, composeTextareaTmpl} {
		if _, err := conversationViewTmpl.AddParseTree(child.Name(), child.Tree); err != nil {
			panic("inbox/web: register " + child.Name() + " in view: " + err.Error())
		}
	}
	for _, child := range []*template.Template{channelBadgeTmpl, conversationAssignmentTmpl} {
		if _, err := conversationContextTmpl.AddParseTree(child.Name(), child.Tree); err != nil {
			panic("inbox/web: register " + child.Name() + " in context: " + err.Error())
		}
	}
	for _, child := range []*template.Template{channelBadgeTmpl} {
		if _, err := conversationListTmpl.AddParseTree(child.Name(), child.Tree); err != nil {
			panic("inbox/web: register " + child.Name() + " in list: " + err.Error())
		}
	}
	for _, child := range []*template.Template{channelBadgeTmpl} {
		if _, err := customerPanelTmpl.AddParseTree(child.Name(), child.Tree); err != nil {
			panic("inbox/web: register " + child.Name() + " in customer: " + err.Error())
		}
	}

	// Prime html/template's lazy escaper on every template now, before any
	// concurrent goroutine can race on the first Execute call. The escaper
	// mutates internal state on first execution; warming it here (single-
	// goroutine init) makes all subsequent concurrent executions read-only.
	for _, t := range []*template.Template{
		messageBubbleTmpl,
		composeTextareaTmpl,
		conversationListTmpl,
		inboxFiltersTmpl,
		inboxListRegionTmpl,
		conversationViewTmpl,
		conversationContextTmpl,
		conversationAssignmentTmpl,
		customerPanelTmpl,
		channelBadgeTmpl,
		inboxLayoutTmpl,
	} {
		_ = t.Execute(io.Discard, nil)
	}
}
