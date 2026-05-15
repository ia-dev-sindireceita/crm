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
	"relativeTime":  relativeTime,
	"messageClass":  messageClass,
	"truncate":      truncate,
	"isFinalStatus": isFinalStatus,
	"statusGlyph":   statusGlyph,
	"statusLabel":   statusLabel,
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

// inboxLayoutTmpl is the full-page shell. Two panes side-by-side: the
// left lists conversations, the right shows the selected conversation
// (or an empty state). The CSRF token is rendered into both <meta>
// (for HTMX) and the outbound form's hidden field.
var inboxLayoutTmpl = template.Must(template.New("inbox.layout").Funcs(templateFuncs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Inbox</title>
  {{.CSRFMeta}}
  <link rel="stylesheet" href="/static/css/inbox.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="inbox-shell" role="main">
    <nav class="inbox-list-pane" aria-label="Conversas">
      {{template "conversation_list" .List}}
    </nav>
    <section id="inbox-conversation-pane" class="inbox-conversation-pane" aria-live="polite">
      <p class="inbox-empty">Selecione uma conversa.</p>
    </section>
  </main>
</body>
</html>
`))

// conversationListTmpl is the standalone "list of conversations"
// partial. The layout embeds it on first render; subsequent HTMX
// requests reuse the same template name so the swap surface is
// identical.
var conversationListTmpl = template.Must(template.New("conversation_list").Funcs(templateFuncs).Parse(`<ul class="conversation-list" role="list">
  {{range .Items}}
  <li class="conversation-list__item" role="listitem">
    <a href="/inbox/conversations/{{.ID}}"
       hx-get="/inbox/conversations/{{.ID}}"
       hx-target="#inbox-conversation-pane"
       hx-swap="innerHTML"
       hx-push-url="true"
       class="conversation-list__link">
      <span class="conversation-list__channel">{{.Channel}}</span>
      <span class="conversation-list__snippet">{{truncate .Snippet 80}}</span>
      <time class="conversation-list__time" datetime="{{.LastMessageAt.Format "2006-01-02T15:04:05Z07:00"}}">{{relativeTime .LastMessageAt}}</time>
    </a>
  </li>
  {{else}}
  <li class="conversation-list__empty">Nenhuma conversa.</li>
  {{end}}
</ul>
`))

// conversationViewTmpl is the right-pane HTMX partial for a single
// conversation. The thread of message_bubble blocks is followed by the
// outbound form (CSRF + hidden conversation id + textarea).
var conversationViewTmpl = template.Must(template.New("conversation_view").Funcs(templateFuncs).Parse(`<article class="conversation" aria-label="Conversa">
  <header class="conversation__header">
    <h1 class="conversation__title">Conversa</h1>
    <span class="conversation__channel">{{.Channel}}</span>
  </header>
  <ol id="conversation-thread" class="conversation__thread" role="list">
    {{range .Messages}}
      {{template "message_bubble" .}}
    {{end}}
  </ol>
  <form class="conversation__compose"
        hx-post="/inbox/conversations/{{.ConversationID}}/messages"
        hx-target="#conversation-thread"
        hx-swap="beforeend"
        hx-on::after-request="this.reset()">
    {{.CSRFInput}}
    <label for="compose-body" class="visually-hidden">Mensagem</label>
    <textarea id="compose-body" name="body" rows="3" maxlength="4096" required
              placeholder="Escreva sua resposta…"></textarea>
    <button type="submit">Enviar</button>
  </form>
</article>
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
  <time class="message-bubble__time" datetime="{{.CreatedAt.Format "2006-01-02T15:04:05Z07:00"}}">{{relativeTime .CreatedAt}}</time>
  {{- if eq .Direction "out"}}
  {{- $glyph := statusGlyph .Status}}{{if $glyph}}
  <span class="message-bubble__status message-bubble__status--{{.Status}}" aria-label="{{statusLabel .Status}}">{{$glyph}}</span>
  {{- end}}
  {{- end}}
</li>
`))

func init() {
	// Cross-register partials so the layout can render
	// {{template "conversation_list" …}} and so on with one template
	// tree. Errors here are programmer errors (typos in the template
	// source) — surface them at process start, not at request time.
	for _, child := range []*template.Template{conversationListTmpl, conversationViewTmpl, messageBubbleTmpl} {
		if _, err := inboxLayoutTmpl.AddParseTree(child.Name(), child.Tree); err != nil {
			panic("inbox/web: register " + child.Name() + ": " + err.Error())
		}
	}
	for _, child := range []*template.Template{messageBubbleTmpl} {
		if _, err := conversationViewTmpl.AddParseTree(child.Name(), child.Tree); err != nil {
			panic("inbox/web: register " + child.Name() + " in view: " + err.Error())
		}
	}

	// Prime html/template's lazy escaper on every template now, before any
	// concurrent goroutine can race on the first Execute call. The escaper
	// mutates internal state on first execution; warming it here (single-
	// goroutine init) makes all subsequent concurrent executions read-only.
	for _, t := range []*template.Template{
		messageBubbleTmpl,
		conversationListTmpl,
		conversationViewTmpl,
		inboxLayoutTmpl,
	} {
		_ = t.Execute(io.Discard, nil)
	}
}
