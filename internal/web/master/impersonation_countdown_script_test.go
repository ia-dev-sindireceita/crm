package master

import (
	"bytes"
	"testing"

	"github.com/pericles-luz/crm/internal/web/shell"
)

// TestMasterLayouts_LoadImpersonationCountdownScript pins SIN-65379 /
// UX-F10. The impersonation banner renders on every authed /master/*
// page (SIN-65369) and carries a <span data-impersonation-countdown>
// whose live value is ticked by /static/js/impersonation-countdown.js.
// Master console pages load only htmx.min.js, so without an explicit
// <script src> the countdown pill stays blank — the original F10
// defect. Pin the exact tag in every master full-page layout so a
// future refactor that drops it fails CI instead of silently
// regressing the countdown.
//
// The script is external + defer, served from 'self', so it loads
// under the strict-CSP policy (script-src 'self' 'nonce-…') without a
// nonce — same CSP-safe pattern as master-grants.js (SIN-63977).
func TestMasterLayouts_LoadImpersonationCountdownScript(t *testing.T) {
	t.Parallel()

	const wantTag = `<script src="/static/js/impersonation-countdown.js" defer></script>`

	cases := []struct {
		name string
		exec func(*bytes.Buffer) error
	}{
		{
			name: "tenants_layout",
			exec: func(buf *bytes.Buffer) error {
				return masterLayoutTmpl.Execute(buf, pageData{})
			},
		},
		{
			name: "billing_layout",
			exec: func(buf *bytes.Buffer) error {
				return billingLayoutTmpl.Execute(buf, billingPageData{})
			},
		},
		{
			name: "grant_requests_layout",
			exec: func(buf *bytes.Buffer) error {
				return grantRequestsLayoutTmpl.Execute(buf, grantRequestsListData{})
			},
		},
		{
			name: "grants_layout",
			exec: func(buf *bytes.Buffer) error {
				return grantsLayoutTmpl.Execute(buf, grantsPageData{})
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
			if !bytes.Contains(buf.Bytes(), []byte(wantTag)) {
				t.Fatalf("%s missing %q — impersonation countdown will not tick under strict CSP", tc.name, wantTag)
			}
		})
	}
}

// TestImpersonationBanner_CountdownNoInlineHandler pins that the
// rendered banner never carries an inline on*="…" event handler for
// the countdown (or anything else). The strict-CSP middleware blocks
// inline handlers at runtime, so the countdown MUST be wired via the
// external script, not an attribute. Mirrors the SEC-F1 guard in
// grants_csp_inline_handlers_test.go.
func TestImpersonationBanner_CountdownNoInlineHandler(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	data := struct {
		ActiveImpersonation *shell.ImpersonationContext
	}{
		ActiveImpersonation: &shell.ImpersonationContext{
			TenantName: "Acme",
			Reason:     "support",
		},
	}
	if err := impersonationBannerTmpl.ExecuteTemplate(&buf, "shell_impersonation_banner", data); err != nil {
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
		t.Fatalf("inline event-handler attribute leaked into impersonation banner:\n  fragment: %s\nstrict-CSP blocks these — the countdown is wired via /static/js/impersonation-countdown.js instead",
			rendered[ctxStart:ctxEnd])
	}
}
