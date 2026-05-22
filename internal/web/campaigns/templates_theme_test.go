package campaigns

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestLayouts_RenderTenantThemeStyle pins the SIN-63092 wireup: every
// full-page template in this package emits the <style id="tenant-theme">
// slot inside <head> when the view's TenantThemeStyle is populated.
// Mirrors views_test.go::TestLayout_RendersTenantThemeStyle.
func TestLayouts_RenderTenantThemeStyle(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	// SIN-63275: the tenant-theme tag now always carries `nonce="…"`.
	// View data here doesn't set CSPNonce, so the render emits
	// `nonce=""` (fail-closed when middleware is absent). The
	// nonce-present case is covered by TestLayouts_StampCSPNonce.
	wantTag := `<style id="tenant-theme" nonce="">` + string(style) + `</style>`

	cases := []struct {
		name string
		tmpl *template.Template
		view any
	}{
		{
			name: "list",
			tmpl: listLayoutTmpl,
			view: listView{TenantThemeStyle: style},
		},
		{
			name: "form",
			tmpl: formLayoutTmpl,
			view: formView{TenantThemeStyle: style},
		},
		{
			name: "detail",
			tmpl: detailLayoutTmpl,
			view: detailView{TenantThemeStyle: style},
		},
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

// TestLayouts_OmitTenantThemeStyleWhenEmpty pins the {{with}} guard so
// a zero-value TenantThemeStyle does NOT emit an empty <style> tag.
func TestLayouts_OmitTenantThemeStyleWhenEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tmpl *template.Template
		view any
	}{
		{name: "list", tmpl: listLayoutTmpl, view: listView{}},
		{name: "form", tmpl: formLayoutTmpl, view: formView{}},
		{name: "detail", tmpl: detailLayoutTmpl, view: detailView{}},
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
