package customdomain

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestBaseTemplate_RendersTenantThemeStyle pins the SIN-63092 wireup:
// when pageData.TenantThemeStyle is non-empty, the base layout emits
// the <style id="tenant-theme"> slot inside <head>.
//
// Parsed standalone (not via loadTemplates) so this test does not race
// with the sync.Once gate other tests in the package may have already
// consumed.
func TestBaseTemplate_RendersTenantThemeStyle(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	// SIN-63275: the tenant-theme tag now always carries `nonce="…"`.
	// pageData here is initialised without Nonce, so the render emits
	// `nonce=""` (fail-closed when middleware is absent). Other
	// customdomain tests in the package set Nonce explicitly and assert
	// the populated attribute via the rendered <script> tags.
	wantTag := `<style id="tenant-theme" nonce="">` + string(style) + `</style>`
	tmpl := parseBaseForTest(t)

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "base", pageData{
		Title:            "Domínios personalizados",
		TenantThemeStyle: style,
	}); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	if !strings.Contains(buf.String(), wantTag) {
		t.Fatalf("missing tenant theme tag.\nwant fragment: %q\nrendered: %q", wantTag, buf.String())
	}
}

// TestBaseTemplate_OmitsTenantThemeStyleWhenEmpty pins the {{with}}
// guard so a zero-value template.CSS does NOT emit an empty <style>
// tag.
func TestBaseTemplate_OmitsTenantThemeStyleWhenEmpty(t *testing.T) {
	t.Parallel()
	tmpl := parseBaseForTest(t)

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "base", pageData{
		Title: "Domínios personalizados",
	}); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	if strings.Contains(buf.String(), `id="tenant-theme"`) {
		t.Fatalf("empty TenantThemeStyle must not emit <style> tag: %q", buf.String())
	}
}

// parseBaseForTest parses the embedded templates with a stub vendor
// provider so the standalone test does not depend on the shared
// sync.Once-gated parser owned by the package.
func parseBaseForTest(t *testing.T) *template.Template {
	t.Helper()
	stub := &stubProvider{attr: `integrity="sha384-XX" crossorigin="anonymous"`}
	tmpl, err := template.New("base").Funcs(buildFuncMap(stub)).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		t.Fatalf("ParseFS: %v", err)
	}
	return tmpl
}
