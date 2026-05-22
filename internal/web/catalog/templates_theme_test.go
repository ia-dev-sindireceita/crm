package catalog

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestCatalogLayouts_RenderTenantThemeStyle pins the SIN-63092 wireup.
func TestCatalogLayouts_RenderTenantThemeStyle(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	// SIN-63275: the tenant-theme tag now always carries `nonce="…"`.
	// View data here doesn't set CSPNonce, so the render emits
	// `nonce=""` (fail-closed when middleware is absent). The
	// nonce-present case is covered by TestCatalogLayouts_StampCSPNonce.
	wantTag := `<style id="tenant-theme" nonce="">` + string(style) + `</style>`

	cases := []struct {
		name string
		tmpl *template.Template
		view any
	}{
		{name: "page", tmpl: pageTmpl, view: pageData{TenantThemeStyle: style}},
		{name: "detail", tmpl: detailTmpl, view: detailData{TenantThemeStyle: style}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := tc.tmpl.Execute(&buf, tc.view); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !strings.Contains(buf.String(), wantTag) {
				t.Fatalf("missing tenant theme tag.\nwant fragment: %q\nrendered: %q", wantTag, buf.String())
			}
		})
	}
}

// TestCatalogLayouts_OmitTenantThemeStyleWhenEmpty pins the {{with}} guard.
func TestCatalogLayouts_OmitTenantThemeStyleWhenEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tmpl *template.Template
		view any
	}{
		{name: "page", tmpl: pageTmpl, view: pageData{}},
		{name: "detail", tmpl: detailTmpl, view: detailData{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := tc.tmpl.Execute(&buf, tc.view); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if strings.Contains(buf.String(), `id="tenant-theme"`) {
				t.Fatalf("empty TenantThemeStyle must not emit <style> tag: %q", buf.String())
			}
		})
	}
}
