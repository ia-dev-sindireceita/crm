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

// boardCSPView is the minimal view shape the head_extra block needs:
// shellCSPNonce reflects the CSPNonce field, and the layout chrome reads
// the tenant-theme fields. Shared by the head_extra guard tests below.
type boardCSPView struct {
	Board            boardView
	CSRFMeta         template.HTML
	HXHeaders        template.HTMLAttr
	CSRFToken        string
	TenantThemeStyle template.CSS
	CSPNonce         string
}

// TestBoardLayout_HTMXConfigDisablesIndicatorStyles pins SIN-65143: the
// funnel head emits <meta name="htmx-config" ...> with
// includeIndicatorStyles:false so htmx does NOT inject its own un-nonced
// inline <style> at init, which the strict CSP (style-src 'self'
// 'nonce-…', no 'unsafe-inline') would otherwise block in the browser.
// The .htmx-indicator defaults are restated in web/static/css/funnel.css,
// served from 'self'. Mirrors the inbox fix (SIN-65046).
func TestBoardLayout_HTMXConfigDisablesIndicatorStyles(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := boardLayoutTmpl.Execute(&buf, boardCSPView{CSPNonce: "n"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	const wantMeta = `<meta name="htmx-config" content='{"includeIndicatorStyles":false}'>`
	if !strings.Contains(buf.String(), wantMeta) {
		t.Fatalf("missing htmx-config meta tag.\nwant fragment: %q\nrendered: %q", wantMeta, buf.String())
	}
}

// TestBoardLayout_HTMXScriptHasNonce pins SIN-65143: the funnel head's
// htmx + funnel-board scripts carry the per-request CSP nonce, matching
// the strict script-src 'self' 'nonce-…' policy and the inbox pattern.
func TestBoardLayout_HTMXScriptHasNonce(t *testing.T) {
	t.Parallel()
	const nonce = "test-csp-nonce-funnel-script"
	var buf bytes.Buffer
	if err := boardLayoutTmpl.Execute(&buf, boardCSPView{CSPNonce: nonce}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := buf.String()
	wantHTMX := `<script src="/static/vendor/htmx/2.0.9/htmx.min.js" nonce="` + nonce + `" defer></script>`
	if !strings.Contains(out, wantHTMX) {
		t.Fatalf("missing nonce on htmx script.\nwant fragment: %q\nrendered: %q", wantHTMX, out)
	}
	wantBoard := `<script src="/static/js/funnel-board.js" nonce="` + nonce + `" defer></script>`
	if !strings.Contains(out, wantBoard) {
		t.Fatalf("missing nonce on funnel-board script.\nwant fragment: %q\nrendered: %q", wantBoard, out)
	}
}
