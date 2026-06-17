package master

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// inlineHandlerRE matches any HTML attribute named on<word>="…".
// Catches onclick, onchange, onsubmit, onload, onfocus, onblur, etc.
// — i.e. every classic inline DOM-event-handler attribute. Used by
// CSP-conformance tests to guarantee no template emits such an
// attribute (the strict-CSP middleware in
// internal/http/middleware/csp/csp.go bans `unsafe-inline` /
// `unsafe-eval`, which blocks inline handlers at runtime).
var inlineHandlerRE = regexp.MustCompile(`(?i)[\s"']on[a-z]+\s*=\s*["']`)

// TestGrantsTemplates_NoInlineEventHandlers pins SIN-63977 / SEC-F1
// for the master grants surface. The grants layout + panel templates
// render to HTML that must NOT carry inline `on*="…"` attributes,
// because the strict-CSP policy refuses to execute them and the
// previous `onclick=` toggle on the kind radios silently became a
// dead button under that policy.
func TestGrantsTemplates_NoInlineEventHandlers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		exec func(*bytes.Buffer) error
	}{
		{
			name: "grants_layout",
			exec: func(buf *bytes.Buffer) error {
				return grantsLayoutTmpl.Execute(buf, grantsPageData{Kind: "free_subscription_period"})
			},
		},
		{
			name: "grants_panel",
			exec: func(buf *bytes.Buffer) error {
				return grantsPanelTmpl.Execute(buf, grantsPageData{Kind: "extra_tokens"})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := tc.exec(&buf); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			rendered := buf.String()
			if loc := inlineHandlerRE.FindStringIndex(rendered); loc != nil {
				ctxStart := loc[0] - 40
				if ctxStart < 0 {
					ctxStart = 0
				}
				ctxEnd := loc[1] + 80
				if ctxEnd > len(rendered) {
					ctxEnd = len(rendered)
				}
				t.Fatalf("inline event-handler attribute leaked into rendered output:\n  fragment: %s\nstrict-CSP (script-src 'self' 'nonce-…') blocks these at runtime — wire the behaviour via a nonced/external script and htmx triggers instead",
					rendered[ctxStart:ctxEnd])
			}
		})
	}
}

// TestGrantsLayout_LoadsMasterGrantsJS pins that the grants page
// loads the external /static/js/master-grants.js file that wires the
// kind-radio toggle previously inlined as `onclick=`. The script must
// be referenced from the layout (not the panel partial) so HTMX swaps
// of #grants-panel don't re-fetch it, and must use `defer` so it
// executes after #grants-panel is parsed.
func TestGrantsLayout_LoadsMasterGrantsJS(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := grantsLayoutTmpl.Execute(&buf, grantsPageData{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	rendered := buf.String()
	wantTag := `<script src="/static/js/master-grants.js" defer></script>`
	if !strings.Contains(rendered, wantTag) {
		t.Fatalf("grants layout missing %q — kind-toggle wiring will not load under strict CSP", wantTag)
	}
}

// TestGrantsPanel_KindRadiosCarryToggleMarkers pins that the radios
// keep `name="kind"` + `data-grant-kind-toggle="…"` so the
// event-delegation selector in /static/js/master-grants.js
// (`input[name="kind"][data-grant-kind-toggle]`) still matches after
// the refactor. Without these data attributes the external script is
// dead and the visible fieldset stays frozen on whichever side the
// server rendered.
func TestGrantsPanel_KindRadiosCarryToggleMarkers(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := grantsPanelTmpl.Execute(&buf, grantsPageData{Kind: "free_subscription_period"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	rendered := buf.String()
	for _, needle := range []string{
		`name="kind"`,
		`value="free_subscription_period"`,
		`value="extra_tokens"`,
		`data-grant-kind-toggle="free"`,
		`data-grant-kind-toggle="extra"`,
		`data-grant-fields="free_subscription_period"`,
		`data-grant-fields="extra_tokens"`,
	} {
		if !strings.Contains(rendered, needle) {
			t.Errorf("grants panel missing %q — master-grants.js delegation selector will not match", needle)
		}
	}
}
