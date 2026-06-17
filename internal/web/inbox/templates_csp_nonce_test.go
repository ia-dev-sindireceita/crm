package inbox

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestInboxLayout_StampCSPNonce pins SIN-63275: the full-page inbox
// layout emits the <style id="tenant-theme"> with `nonce="{{$.CSPNonce}}"`
// so the strict `style-src 'self' 'nonce-…'` policy (no
// `'unsafe-inline'`) accepts the inline tag in the browser.
func TestInboxLayout_StampCSPNonce(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	const nonce = "test-csp-nonce-inbox"
	wantTag := `<style id="tenant-theme" nonce="` + nonce + `">` + string(style) + `</style>`
	var buf bytes.Buffer
	if err := inboxLayoutTmpl.Execute(&buf, layoutData{TenantThemeStyle: style, CSPNonce: nonce}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), wantTag) {
		t.Fatalf("missing tenant-theme tag with nonce.\nwant fragment: %q\nrendered: %q", wantTag, buf.String())
	}
}

// TestInboxLayout_HTMXConfigDisablesIndicatorStyles pins SIN-65046: the
// layout emits <meta name="htmx-config" ...> with
// includeIndicatorStyles:false so htmx does NOT inject its own un-nonced
// inline <style> at init, which the strict CSP (style-src 'self'
// 'nonce-…', no 'unsafe-inline') would otherwise block in the browser.
func TestInboxLayout_HTMXConfigDisablesIndicatorStyles(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := inboxLayoutTmpl.Execute(&buf, layoutData{CSPNonce: "n"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	const wantMeta = `<meta name="htmx-config" content='{"includeIndicatorStyles":false}'>`
	if !strings.Contains(buf.String(), wantMeta) {
		t.Fatalf("missing htmx-config meta tag.\nwant fragment: %q\nrendered: %q", wantMeta, buf.String())
	}
}

// TestInboxLayout_HTMXScriptHasNonce pins SIN-65046: the htmx <script>
// carries the per-request CSP nonce, matching the strict
// script-src policy used elsewhere (customdomain/templates/base.html).
func TestInboxLayout_HTMXScriptHasNonce(t *testing.T) {
	t.Parallel()
	const nonce = "test-csp-nonce-script"
	var buf bytes.Buffer
	if err := inboxLayoutTmpl.Execute(&buf, layoutData{CSPNonce: nonce}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	wantScript := `<script src="/static/vendor/htmx/2.0.9/htmx.min.js" nonce="` + nonce + `" defer></script>`
	if !strings.Contains(buf.String(), wantScript) {
		t.Fatalf("missing htmx script with nonce.\nwant fragment: %q\nrendered: %q", wantScript, buf.String())
	}
}
