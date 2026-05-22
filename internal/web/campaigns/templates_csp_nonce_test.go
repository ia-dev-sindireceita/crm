package campaigns

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestLayouts_StampCSPNonce pins SIN-63275 across every full-page
// campaigns template. Each render emits the <style id="tenant-theme">
// with `nonce="{{$.CSPNonce}}"` so the strict `style-src 'self'
// 'nonce-…'` policy (no `'unsafe-inline'`) accepts the inline tag in
// the browser.
func TestLayouts_StampCSPNonce(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	const nonce = "test-csp-nonce-campaigns"
	wantTag := `<style id="tenant-theme" nonce="` + nonce + `">` + string(style) + `</style>`

	cases := []struct {
		name string
		tmpl *template.Template
		view any
	}{
		{name: "list", tmpl: listLayoutTmpl, view: listView{TenantThemeStyle: style, CSPNonce: nonce}},
		{name: "form", tmpl: formLayoutTmpl, view: formView{TenantThemeStyle: style, CSPNonce: nonce}},
		{name: "detail", tmpl: detailLayoutTmpl, view: detailView{TenantThemeStyle: style, CSPNonce: nonce}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := tc.tmpl.Execute(&buf, tc.view); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !strings.Contains(buf.String(), wantTag) {
				t.Fatalf("missing tenant-theme tag with nonce.\nwant fragment: %q\nrendered: %q", wantTag, buf.String())
			}
		})
	}
}
