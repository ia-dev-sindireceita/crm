package funnel

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestBoardLayout_StampCSPNonce pins SIN-63275: the full-page funnel
// layout emits the <style id="tenant-theme"> with `nonce="{{$.CSPNonce}}"`
// so the strict `style-src 'self' 'nonce-…'` policy (no
// `'unsafe-inline'`) accepts the inline tag in the browser.
func TestBoardLayout_StampCSPNonce(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	const nonce = "test-csp-nonce-funnel"
	wantTag := `<style id="tenant-theme" nonce="` + nonce + `">` + string(style) + `</style>`
	var buf bytes.Buffer
	view := struct {
		Board            boardView
		CSRFMeta         template.HTML
		HXHeaders        template.HTMLAttr
		CSRFToken        string
		TenantThemeStyle template.CSS
		CSPNonce         string
	}{TenantThemeStyle: style, CSPNonce: nonce}
	if err := boardLayoutTmpl.Execute(&buf, view); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), wantTag) {
		t.Fatalf("missing tenant-theme tag with nonce.\nwant fragment: %q\nrendered: %q", wantTag, buf.String())
	}
}
