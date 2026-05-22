package lgpd

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestLgpdLayouts_StampCSPNonce pins SIN-63275: every full-page LGPD
// template emits the <style id="tenant-theme"> slot with
// `nonce="{{$.CSPNonce}}"` so the strict `style-src 'self' 'nonce-…'`
// policy (no `'unsafe-inline'`) accepts the inline tag in the browser.
func TestLgpdLayouts_StampCSPNonce(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	const nonce = "test-csp-nonce-lgpd"
	wantTag := `<style id="tenant-theme" nonce="` + nonce + `">` + string(style) + `</style>`

	cases := []struct {
		name string
		tmpl *template.Template
		view any
	}{
		{name: "contact", tmpl: contactLayoutTmpl, view: contactPageData{TenantThemeStyle: style, CSPNonce: nonce}},
		{name: "requests", tmpl: requestsLayoutTmpl, view: requestsPageData{TenantThemeStyle: style, CSPNonce: nonce}},
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

// TestLgpdLayouts_RenderTenantThemeStyleFailClosed pins the fail-closed
// path: when CSPNonce is empty (middleware absent), the template still
// emits `nonce=""` so the browser blocks the inline tag.
func TestLgpdLayouts_RenderTenantThemeStyleFailClosed(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	wantTag := `<style id="tenant-theme" nonce="">` + string(style) + `</style>`

	cases := []struct {
		name string
		tmpl *template.Template
		view any
	}{
		{name: "contact", tmpl: contactLayoutTmpl, view: contactPageData{TenantThemeStyle: style}},
		{name: "requests", tmpl: requestsLayoutTmpl, view: requestsPageData{TenantThemeStyle: style}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := tc.tmpl.Execute(&buf, tc.view); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !strings.Contains(buf.String(), wantTag) {
				t.Fatalf("missing fail-closed tenant-theme tag.\nwant fragment: %q\nrendered: %q", wantTag, buf.String())
			}
		})
	}
}
