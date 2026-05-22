package aipolicy

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestPageLayout_RendersTenantThemeStyle pins the SIN-63092 wireup.
func TestPageLayout_RendersTenantThemeStyle(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	// SIN-63275: the tenant-theme tag now always carries `nonce="…"`.
	// The nonce-present case is covered by TestPageLayout_StampCSPNonce.
	wantTag := `<style id="tenant-theme" nonce="">` + string(style) + `</style>`
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, pageData{TenantThemeStyle: style}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), wantTag) {
		t.Fatalf("missing tenant theme tag.\nwant fragment: %q\nrendered: %q", wantTag, buf.String())
	}
}

// TestPageLayout_OmitsTenantThemeStyleWhenEmpty pins the {{with}} guard.
func TestPageLayout_OmitsTenantThemeStyleWhenEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, pageData{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(buf.String(), `id="tenant-theme"`) {
		t.Fatalf("empty TenantThemeStyle must not emit <style> tag: %q", buf.String())
	}
}
