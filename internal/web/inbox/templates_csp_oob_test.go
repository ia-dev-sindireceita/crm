package inbox

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestConversationView_NoHxOnUnderStrictCSP pins SIN-65068: the compose
// form must NOT emit any `hx-on` attribute. htmx compiles every hx-on:*
// value with new Function(...), which throws EvalError under the prod
// strict CSP (script-src without 'unsafe-eval'). The form reset is now
// handled by an out-of-band textarea swap from the send handler instead.
func TestConversationView_NoHxOnUnderStrictCSP(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := conversationViewTmpl.Execute(&buf, viewData{
		ConversationID: uuid.New(),
		Channel:        "whatsapp",
		CSRFInput:      `<input type="hidden" name="_csrf" value="tok">`,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "hx-on") {
		t.Errorf("conversation_view still emits an hx-on attribute (eval under strict CSP):\n%s", out)
	}
	// The in-form textarea must NOT carry hx-swap-oob — only the standalone
	// reset fragment does. A live form field with hx-swap-oob would be
	// stripped from the DOM by htmx on every settle.
	if strings.Contains(out, `hx-swap-oob`) {
		t.Errorf("in-form textarea must not carry hx-swap-oob:\n%s", out)
	}
	for _, want := range []string{`id="compose-body"`, `name="body"`} {
		if !strings.Contains(out, want) {
			t.Errorf("conversation_view missing %q", want)
		}
	}
}

// TestComposeTextarea_OOBFragment pins the shared compose_textarea
// template: dot=true emits the out-of-band reset fragment keyed by
// id="compose-body"; dot=false emits the in-form variant without it. The
// single source keeps the two renders from drifting.
func TestComposeTextarea_OOBFragment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		oob    bool
		wantUB bool // expect hx-swap-oob present
	}{
		{name: "oob reset fragment", oob: true, wantUB: true},
		{name: "in-form variant", oob: false, wantUB: false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := composeTextareaTmpl.Execute(&buf, tc.oob); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			out := buf.String()
			if got := strings.Contains(out, `hx-swap-oob="true"`); got != tc.wantUB {
				t.Errorf("hx-swap-oob present=%v want=%v:\n%s", got, tc.wantUB, out)
			}
			if !strings.Contains(out, `id="compose-body"`) {
				t.Errorf("fragment missing id=compose-body:\n%s", out)
			}
			if !strings.Contains(out, `name="body"`) {
				t.Errorf("fragment missing name=body:\n%s", out)
			}
		})
	}
}
