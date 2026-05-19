package invoices

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestLayouts_RenderTenantThemeStyle pins the SIN-63092 wireup: every
// full-page billing/invoices template emits the <style id="tenant-theme">
// slot inside <head> when the view's TenantThemeStyle is populated.
func TestLayouts_RenderTenantThemeStyle(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	wantTag := `<style id="tenant-theme">` + string(style) + `</style>`

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
			name: "detail",
			tmpl: detailLayoutTmpl,
			view: detailView{
				TenantThemeStyle: style,
				Status:           statusFragment{Status: "pending", Label: "aguardando"},
			},
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

// TestLayouts_OmitTenantThemeStyleWhenEmpty pins the {{with}} guard.
func TestLayouts_OmitTenantThemeStyleWhenEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tmpl *template.Template
		view any
	}{
		{name: "list", tmpl: listLayoutTmpl, view: listView{}},
		{
			name: "detail",
			tmpl: detailLayoutTmpl,
			view: detailView{Status: statusFragment{Status: "pending", Label: "aguardando"}},
		},
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
