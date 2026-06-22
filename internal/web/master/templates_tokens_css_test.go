package master

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

// TestMasterLayouts_LinkTokensBeforeMasterCSS pins SIN-65124: every
// full-page master-console layout must link the Pitho design-token
// sheet (/static/css/tokens.css) BEFORE /static/css/master.css.
//
// master.css consumes design tokens via bare `var(--*)` (no fallbacks)
// and its header even states "Every value comes from a design token in
// tokens.css". With tokens.css absent every reference resolved empty,
// so the console rendered as raw serif HTML (collapsed spacing, UA
// default colors, default form controls). tokens.css must come first so
// the custom properties — plus the shared `.visually-hidden` helper and
// the `[data-theme="dark"]` rebind, both defined in tokens.css — are in
// scope before master.css reads them.
func TestMasterLayouts_LinkTokensBeforeMasterCSS(t *testing.T) {
	t.Parallel()

	const (
		tokensLink = `<link rel="stylesheet" href="/static/css/tokens.css">`
		masterLink = `<link rel="stylesheet" href="/static/css/master.css">`
	)

	cases := []struct {
		name string
		tmpl *template.Template
		view any
	}{
		{name: "master_tenants", tmpl: masterLayoutTmpl, view: pageData{}},
		{name: "tenant_detail", tmpl: tenantDetailLayoutTmpl, view: tenantDetailData{}},
		{name: "billing", tmpl: billingLayoutTmpl, view: billingPageData{}},
		{name: "ledger", tmpl: ledgerLayoutTmpl, view: ledgerPageData{}},
		{name: "grant_requests", tmpl: grantRequestsLayoutTmpl, view: grantRequestsListData{}},
		{name: "grant_request_detail", tmpl: grantRequestDetailLayoutTmpl, view: grantRequestDetailData{}},
		{name: "grants", tmpl: grantsLayoutTmpl, view: grantsPageData{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := tc.tmpl.Execute(&buf, tc.view); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			out := buf.String()

			tokensAt := strings.Index(out, tokensLink)
			masterAt := strings.Index(out, masterLink)
			if tokensAt < 0 {
				t.Fatalf("missing tokens.css link.\nwant fragment: %q\nrendered: %q", tokensLink, out)
			}
			if masterAt < 0 {
				t.Fatalf("missing master.css link.\nwant fragment: %q\nrendered: %q", masterLink, out)
			}
			if tokensAt > masterAt {
				t.Fatalf("tokens.css must be linked BEFORE master.css (tokens@%d, master@%d):\n%s", tokensAt, masterAt, out)
			}
		})
	}
}
