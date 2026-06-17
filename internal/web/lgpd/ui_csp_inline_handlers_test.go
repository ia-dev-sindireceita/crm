package lgpd

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// inlineHandlerRE matches any HTML attribute named on<word>="…". See
// the matching constant in internal/web/master for the same regex; this
// is duplicated here to keep each test package self-contained without
// pulling in a shared internal helper.
var inlineHandlerRE = regexp.MustCompile(`(?i)[\s"']on[a-z]+\s*=\s*["']`)

// TestRequestsTemplates_NoInlineEventHandlers pins SIN-63977 / SEC-F1
// for the LGPD admin surface. Both the full-page layout and the
// partial swapped by the filter form must render to HTML without
// `on*="…"` attributes — those silently became dead under the strict
// CSP (`script-src 'self' 'nonce-…'`, no `unsafe-inline`).
func TestRequestsTemplates_NoInlineEventHandlers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		exec func(*bytes.Buffer) error
	}{
		{
			name: "requests_layout",
			exec: func(buf *bytes.Buffer) error {
				return requestsLayoutTmpl.Execute(buf, requestsPageData{
					Panel: requestsPanelData{Filters: statusFilters()},
				})
			},
		},
		{
			name: "requests_panel",
			exec: func(buf *bytes.Buffer) error {
				return requestsPanelTmpl.Execute(buf, requestsPanelData{Filters: statusFilters()})
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
				t.Fatalf("inline event-handler attribute leaked into rendered output:\n  fragment: %s\nstrict-CSP (script-src 'self' 'nonce-…') blocks these — wire the behaviour via htmx triggers or a nonced script instead",
					rendered[ctxStart:ctxEnd])
			}
		})
	}
}

// TestRequestsFilterForm_HXTriggerOnChange pins that the filter form
// uses htmx `hx-trigger="submit, change"` instead of an inline
// `onchange="this.form.requestSubmit()"` on the select. Without the
// `change` trigger, picking a new status no longer auto-submits and
// the noscript "Filtrar" button becomes mandatory — a regression
// against the existing UX contract.
func TestRequestsFilterForm_HXTriggerOnChange(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := requestsPanelTmpl.Execute(&buf, requestsPanelData{Filters: statusFilters()}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	rendered := buf.String()
	if !strings.Contains(rendered, `hx-trigger="submit, change"`) {
		t.Fatalf("requests panel form missing `hx-trigger=\"submit, change\"`:\n%s", rendered)
	}
	// Defense in depth: the form must still carry the GET fallback so
	// noscript users can submit it the classic way.
	if !strings.Contains(rendered, `method="get"`) {
		t.Errorf("requests panel form missing method=\"get\" noscript fallback")
	}
}
