package inbox

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// inlineHandlerRe matches a legacy inline DOM event-handler attribute
// (onclick=, onchange=, onsubmit=, …) — the strict prod CSP
// (script-src 'self' 'nonce-…' without 'unsafe-inline') renders these
// but never executes them, so they break silently. The word boundary
// keeps it from matching legitimate words like "button" or "Resumir".
var inlineHandlerRe = regexp.MustCompile(`(?i)\bon[a-z]+\s*=`)

// TestAssistButton_NoInlineHandlersUnderStrictCSP pins the SIN-65244 /
// SIN-63977 contract for the AI-assist *button* fragment (the existing
// ai_assist_csp_test.go covers the result panel + suggestion chips).
// The button drives the request purely with declarative hx-* attributes
// plus an hx-trigger event name; it must emit neither an hx-on:* value
// (htmx compiles those with new Function → EvalError under strict CSP)
// nor any inline on*= handler the CSP would block silently.
func TestAssistButton_NoInlineHandlersUnderStrictCSP(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{true, false} {
		enabled := enabled
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := assistButtonTmpl.Execute(&buf, assistButtonData{
				ConversationID: uuid.New(),
				ChannelID:      "whatsapp",
				TeamID:         "vendas",
				Enabled:        enabled,
			}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			out := buf.String()
			if strings.Contains(out, "hx-on") {
				t.Errorf("assist button emits hx-on (EvalError under strict CSP):\n%s", out)
			}
			if loc := inlineHandlerRe.FindString(out); loc != "" {
				t.Errorf("assist button emits inline handler %q (blocked silently by strict CSP):\n%s", loc, out)
			}
			// The declarative submit path the button relies on must survive.
			for _, want := range []string{`hx-post="/inbox/conversations/`, `hx-trigger="submit`} {
				if !strings.Contains(out, want) {
					t.Errorf("assist button missing declarative hook %q", want)
				}
			}
		})
	}
}
