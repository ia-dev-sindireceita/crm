package invoices

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

// TestLayouts_LinkPithoStylesheets pins SIN-65123: every full-page
// billing/invoices layout must link the shared Pitho sheets
// (tokens.css + components.css) BEFORE its own billing-invoices.css and
// load the copy-to-clipboard script. The page shipped unstyled because
// the template linked billing-invoices.css/.js that never existed; the
// disk-existence half is guarded in cmd/server, this half guards the
// <head> wireup so a future edit can't silently drop a sheet again.
func TestLayouts_LinkPithoStylesheets(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		tmpl *template.Template
		view any
	}{
		{
			name: "list",
			tmpl: listLayoutTmpl,
			view: listView{},
		},
		{
			name: "detail",
			tmpl: detailLayoutTmpl,
			view: detailView{Status: statusFragment{Status: "pending", Label: "aguardando"}},
		},
	}

	wantLinks := []string{
		`<link rel="stylesheet" href="/static/css/tokens.css">`,
		`<link rel="stylesheet" href="/static/css/components.css">`,
		`<link rel="stylesheet" href="/static/css/billing-invoices.css">`,
		`<script src="/static/js/billing-invoices.js" defer></script>`,
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := tc.tmpl.Execute(&buf, tc.view); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			out := buf.String()
			for _, link := range wantLinks {
				if !strings.Contains(out, link) {
					t.Errorf("%s layout missing head asset %q", tc.name, link)
				}
			}
			// tokens.css must precede billing-invoices.css so the page
			// sheet can override design-system defaults, not the reverse.
			if ti, bi := strings.Index(out, "tokens.css"), strings.Index(out, "billing-invoices.css"); ti < 0 || ti > bi {
				t.Errorf("%s layout: tokens.css must be linked before billing-invoices.css (tokens@%d, page@%d)", tc.name, ti, bi)
			}
		})
	}
}
