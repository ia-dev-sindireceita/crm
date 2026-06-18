package inbox

import (
	"bytes"
	"strings"
	"testing"
)

// TestAssistPanel_NoHxOnUnderStrictCSP pins SIN-63977/65097: the
// AI-assist suggestion chips must NOT emit any hx-on/on* attribute.
// htmx compiles hx-on:* values with new Function(...), which throws
// EvalError under the prod strict CSP (script-src without 'unsafe-eval'),
// silently breaking the "usar sugestão" interaction. The fill-compose
// behaviour now lives in a single nonce'd delegated listener in the page
// layout; each chip carries only data-suggestion.
func TestAssistPanel_NoHxOnUnderStrictCSP(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := assistPanelTmpl.Execute(&buf, assistPanelView{
		Summary:     "Cliente quer renegociar o débito.",
		Suggestions: []string{"Olá! Podemos parcelar em até 12x.", "Posso te enviar o boleto agora?"},
		CacheHit:    true,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "hx-on") {
		t.Errorf("assist panel still emits an hx-on attribute (eval under strict CSP):\n%s", out)
	}
	// No legacy inline DOM handler may survive either.
	for _, banned := range []string{"onclick", "this.dataset", "getElementById(", "document."} {
		if strings.Contains(out, banned) {
			t.Errorf("assist panel emits inline script fragment %q (must be in the nonce'd listener, not the swapped fragment):\n%s", banned, out)
		}
	}
	// The declarative hooks the delegated listener depends on must be present.
	for _, want := range []string{`class="ai-assist__suggestion-btn"`, `data-suggestion="`} {
		if !strings.Contains(out, want) {
			t.Errorf("assist panel missing %q", want)
		}
	}
}

// TestInboxLayout_SuggestionListenerIsNonced pins SIN-65097: the compose
// fill-from-suggestion behaviour is served by one delegated click
// listener carried by the full-page layout, nonce'd to satisfy the
// strict script-src 'self' 'nonce-…' policy (no 'unsafe-inline').
func TestInboxLayout_SuggestionListenerIsNonced(t *testing.T) {
	t.Parallel()
	const nonce = "test-csp-nonce-listener"
	var buf bytes.Buffer
	if err := inboxLayoutTmpl.Execute(&buf, layoutData{CSPNonce: nonce}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `<script nonce="`+nonce+`">`) {
		t.Fatalf("layout missing nonce'd <script> for the suggestion listener.\nrendered: %q", out)
	}
	// The listener must delegate on the chip class and target #compose-body.
	for _, want := range []string{".ai-assist__suggestion-btn", "compose-body", "addEventListener"} {
		if !strings.Contains(out, want) {
			t.Errorf("suggestion listener missing %q", want)
		}
	}
}
