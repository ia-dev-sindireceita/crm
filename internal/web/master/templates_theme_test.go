package master

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestMasterLayouts_RenderTenantThemeStyle pins the SIN-63092 wireup:
// every full-page master template emits the <style id="tenant-theme">
// slot inside <head> when the view's TenantThemeStyle is populated.
func TestMasterLayouts_RenderTenantThemeStyle(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	wantTag := `<style id="tenant-theme">` + string(style) + `</style>`

	cases := []struct {
		name string
		tmpl *template.Template
		view any
	}{
		{
			name: "master_tenants",
			tmpl: masterLayoutTmpl,
			view: pageData{TenantThemeStyle: style},
		},
		{
			name: "billing",
			tmpl: billingLayoutTmpl,
			view: billingPageData{TenantThemeStyle: style},
		},
		{
			name: "ledger",
			tmpl: ledgerLayoutTmpl,
			view: ledgerPageData{TenantThemeStyle: style},
		},
		{
			name: "grants",
			tmpl: grantsLayoutTmpl,
			view: grantsPageData{TenantThemeStyle: style},
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

// TestMasterLayouts_OmitTenantThemeStyleWhenEmpty pins the {{with}}
// guard so a zero-value TenantThemeStyle does NOT emit a <style> tag.
func TestMasterLayouts_OmitTenantThemeStyleWhenEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tmpl *template.Template
		view any
	}{
		{name: "master_tenants", tmpl: masterLayoutTmpl, view: pageData{}},
		{name: "billing", tmpl: billingLayoutTmpl, view: billingPageData{}},
		{name: "ledger", tmpl: ledgerLayoutTmpl, view: ledgerPageData{}},
		{name: "grants", tmpl: grantsLayoutTmpl, view: grantsPageData{}},
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
