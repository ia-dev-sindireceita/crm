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
	// SIN-63275: the tenant-theme tag now always carries `nonce="…"`.
	// Cases here don't set CSPNonce on the view data, so renders emit
	// `nonce=""` (fail-closed when middleware is absent). The
	// nonce-present case is covered separately by
	// TestTemplates_StampCSPNonce.
	wantTag := `<style id="tenant-theme" nonce="">` + string(style) + `</style>`

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

// TestTemplates_StampCSPNonce pins SIN-63275 across every mastermfa
// full-page template. Each render emits both the tenant-theme <style>
// and the page-level <style> blocks with `nonce="{{.CSPNonce}}"` so
// the strict `style-src 'self' 'nonce-…'` policy (no
// `'unsafe-inline'`) accepts them in the browser.
func TestTemplates_StampCSPNonce(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	const nonce = "test-csp-nonce-abc"
	wantTenantTag := `<style id="tenant-theme" nonce="` + nonce + `">` + string(style) + `</style>`
	wantPageTag := `<style nonce="` + nonce + `">`

	cases := []struct {
		name    string
		fs      embed.FS
		path    string
		tplName string
		data    any
	}{
		{name: "login", fs: loginTemplates, path: "templates/login.html", tplName: "login.html", data: loginViewData{TenantThemeStyle: style, CSPNonce: nonce}},
		{name: "verify", fs: verifyTemplates, path: "templates/verify.html", tplName: "verify.html", data: verifyViewData{TenantThemeStyle: style, CSPNonce: nonce}},
		{name: "enroll_result", fs: enrollTemplates, path: "templates/enroll_result.html", tplName: "enroll_result.html", data: enrollViewData{TenantThemeStyle: style, CSPNonce: nonce}},
		{name: "regenerate_result", fs: regenerateTemplates, path: "templates/regenerate_result.html", tplName: "regenerate_result.html", data: regenerateViewData{TenantThemeStyle: style, CSPNonce: nonce}},
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
			got := buf.String()
			if !strings.Contains(got, wantTenantTag) {
				t.Fatalf("tenant-theme tag missing nonce.\nwant: %q\nrendered: %q", wantTenantTag, got)
			}
			if !strings.Contains(got, wantPageTag) {
				t.Fatalf("page-level <style> missing nonce.\nwant fragment: %q\nrendered: %q", wantPageTag, got)
			}
			// Sanity: no inline <style> without nonce should slip in.
			if strings.Contains(got, "<style>") {
				t.Fatalf("rendered output contains a <style> tag with no attributes: %q", got)
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
