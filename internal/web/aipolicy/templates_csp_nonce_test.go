package aipolicy

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestPageLayout_StampCSPNonce pins SIN-63275: the full-page aipolicy
// layout emits the <style id="tenant-theme"> with `nonce="{{$.CSPNonce}}"`
// so the strict `style-src 'self' 'nonce-…'` policy (no
// `'unsafe-inline'`) accepts the inline tag in the browser.
func TestPageLayout_StampCSPNonce(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	const nonce = "test-csp-nonce-aipolicy"
	wantTag := `<style id="tenant-theme" nonce="` + nonce + `">` + string(style) + `</style>`
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, pageData{TenantThemeStyle: style, CSPNonce: nonce}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), wantTag) {
		t.Fatalf("missing tenant-theme tag with nonce.\nwant fragment: %q\nrendered: %q", wantTag, buf.String())
	}
}
