package mastermfa

import (
	"bytes"
	"embed"
	"html/template"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestTemplates_RenderTenantThemeStyle pins the SIN-63092 wireup
// across every mastermfa full-page template. Each render emits the
// <style id="tenant-theme"> slot inside <head> when the view-data's
// TenantThemeStyle is non-empty.
func TestTemplates_RenderTenantThemeStyle(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	wantTag := `<style id="tenant-theme">` + string(style) + `</style>`

	cases := []struct {
		name    string
		fs      embed.FS
		path    string
		tplName string
		data    any
	}{
		{name: "login", fs: loginTemplates, path: "templates/login.html", tplName: "login.html", data: loginViewData{TenantThemeStyle: style}},
		{name: "verify", fs: verifyTemplates, path: "templates/verify.html", tplName: "verify.html", data: verifyViewData{TenantThemeStyle: style}},
		{name: "enroll_result", fs: enrollTemplates, path: "templates/enroll_result.html", tplName: "enroll_result.html", data: enrollViewData{TenantThemeStyle: style}},
		{name: "regenerate_result", fs: regenerateTemplates, path: "templates/regenerate_result.html", tplName: "regenerate_result.html", data: regenerateViewData{TenantThemeStyle: style}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpl, err := template.ParseFS(tc.fs, tc.path)
			if err != nil {
				t.Fatalf("ParseFS(%s): %v", tc.path, err)
			}
			var buf bytes.Buffer
			if err := tmpl.ExecuteTemplate(&buf, tc.tplName, tc.data); err != nil {
				t.Fatalf("ExecuteTemplate(%s): %v", tc.tplName, err)
			}
			if !strings.Contains(buf.String(), wantTag) {
				t.Fatalf("missing tenant theme tag.\nwant fragment: %q\nrendered: %q", wantTag, buf.String())
			}
		})
	}
}

// TestTemplates_OmitTenantThemeStyleWhenEmpty pins the {{with}} guard
// across every mastermfa template — zero-value TenantThemeStyle must
// NOT emit a <style id="tenant-theme"> tag.
func TestTemplates_OmitTenantThemeStyleWhenEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		fs      embed.FS
		path    string
		tplName string
		data    any
	}{
		{name: "login", fs: loginTemplates, path: "templates/login.html", tplName: "login.html", data: loginViewData{}},
		{name: "verify", fs: verifyTemplates, path: "templates/verify.html", tplName: "verify.html", data: verifyViewData{}},
		{name: "enroll_result", fs: enrollTemplates, path: "templates/enroll_result.html", tplName: "enroll_result.html", data: enrollViewData{}},
		{name: "regenerate_result", fs: regenerateTemplates, path: "templates/regenerate_result.html", tplName: "regenerate_result.html", data: regenerateViewData{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpl, err := template.ParseFS(tc.fs, tc.path)
			if err != nil {
				t.Fatalf("ParseFS(%s): %v", tc.path, err)
			}
			var buf bytes.Buffer
			if err := tmpl.ExecuteTemplate(&buf, tc.tplName, tc.data); err != nil {
				t.Fatalf("ExecuteTemplate(%s): %v", tc.tplName, err)
			}
			if strings.Contains(buf.String(), `id="tenant-theme"`) {
				t.Fatalf("empty TenantThemeStyle must not emit <style> tag: %q", buf.String())
			}
		})
	}
}
