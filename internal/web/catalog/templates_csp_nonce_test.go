package catalog

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestCatalogLayouts_StampCSPNonce pins SIN-63275 across every
// full-page template in this package. Each render emits the
// <style id="tenant-theme"> with `nonce="{{$.CSPNonce}}"` so the
// strict `style-src 'self' 'nonce-…'` policy (no `'unsafe-inline'`)
// accepts the inline tag in the browser.
func TestCatalogLayouts_StampCSPNonce(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	const nonce = "test-csp-nonce-abc"
	wantTenantTag := `<style id="tenant-theme" nonce="` + nonce + `">` + string(style) + `</style>`

	cases := []struct {
		name string
		tmpl *template.Template
		view any
	}{
		{name: "page", tmpl: pageTmpl, view: pageData{TenantThemeStyle: style, CSPNonce: nonce}},
		{name: "detail", tmpl: detailTmpl, view: detailData{TenantThemeStyle: style, CSPNonce: nonce}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := tc.tmpl.Execute(&buf, tc.view); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !strings.Contains(buf.String(), wantTenantTag) {
				t.Fatalf("missing tenant-theme tag with nonce.\nwant fragment: %q\nrendered: %q", wantTenantTag, buf.String())
			}
		})
	}
}
