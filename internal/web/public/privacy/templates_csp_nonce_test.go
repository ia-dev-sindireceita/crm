package privacy

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestPublicPrivacy_StampCSPNonce pins SIN-63275: the public privacy
// page emits the <style id="tenant-theme"> with `nonce="{{$.CSPNonce}}"`
// so the strict `style-src 'self' 'nonce-…'` policy (no
// `'unsafe-inline'`) accepts the inline tag in the browser.
func TestPublicPrivacy_StampCSPNonce(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	const nonce = "test-csp-nonce-public-privacy"
	wantTag := `<style id="tenant-theme" nonce="` + nonce + `">` + string(style) + `</style>`

	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, pageData{
		TenantName:       "acme",
		TenantThemeStyle: style,
		CSPNonce:         nonce,
		Version:          "v1",
		UpdatedAt:        "2026-05-22",
		UpdatedAtISO:     "2026-05-22T00:00:00Z",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), wantTag) {
		t.Fatalf("missing tenant-theme tag with nonce.\nwant fragment: %q\nrendered: %q", wantTag, buf.String())
	}
}

// TestPublicPrivacy_RenderTenantThemeStyleFailClosed pins the
// fail-closed path: when CSPNonce is empty (middleware absent), the
// template still emits `nonce=""` so the browser blocks the inline tag.
func TestPublicPrivacy_RenderTenantThemeStyleFailClosed(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	wantTag := `<style id="tenant-theme" nonce="">` + string(style) + `</style>`

	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, pageData{
		TenantName:       "acme",
		TenantThemeStyle: style,
		Version:          "v1",
		UpdatedAt:        "2026-05-22",
		UpdatedAtISO:     "2026-05-22T00:00:00Z",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), wantTag) {
		t.Fatalf("missing fail-closed tenant-theme tag.\nwant fragment: %q\nrendered: %q", wantTag, buf.String())
	}
}
