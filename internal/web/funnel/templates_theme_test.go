package funnel

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestBoardLayout_RendersTenantThemeStyle pins the SIN-63092 wireup:
// the full-page funnel layout emits the <style id="tenant-theme">
// slot inside <head> when the view's TenantThemeStyle is populated.
func TestBoardLayout_RendersTenantThemeStyle(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	// SIN-63275: the tenant-theme tag now always carries `nonce="…"`.
	// The nonce-present case is covered by TestBoardLayout_StampCSPNonce.
	wantTag := `<style id="tenant-theme" nonce="">` + string(style) + `</style>`
	var buf bytes.Buffer
	view := struct {
		Board            boardView
		CSRFMeta         template.HTML
		HXHeaders        template.HTMLAttr
		CSRFToken        string
		TenantThemeStyle template.CSS
		CSPNonce         string
	}{TenantThemeStyle: style}
	if err := boardLayoutTmpl.Execute(&buf, view); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), wantTag) {
		t.Fatalf("missing tenant theme tag.\nwant fragment: %q\nrendered: %q", wantTag, buf.String())
	}
}

// TestBoardLayout_OmitsTenantThemeStyleWhenEmpty pins the {{with}} guard.
func TestBoardLayout_OmitsTenantThemeStyleWhenEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	view := struct {
		Board            boardView
		CSRFMeta         template.HTML
		HXHeaders        template.HTMLAttr
		CSRFToken        string
		TenantThemeStyle template.CSS
		CSPNonce         string
	}{}
	if err := boardLayoutTmpl.Execute(&buf, view); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(buf.String(), `id="tenant-theme"`) {
		t.Fatalf("empty TenantThemeStyle must not emit <style> tag: %q", buf.String())
	}
}
