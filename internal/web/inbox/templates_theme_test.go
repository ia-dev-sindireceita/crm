package inbox

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// TestInboxLayout_RendersTenantThemeStyle pins the SIN-63092 wireup:
// the full-page inbox layout emits the <style id="tenant-theme"> slot
// inside <head> when layoutData.TenantThemeStyle is populated.
func TestInboxLayout_RendersTenantThemeStyle(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	wantTag := `<style id="tenant-theme">` + string(style) + `</style>`
	var buf bytes.Buffer
	if err := inboxLayoutTmpl.Execute(&buf, layoutData{TenantThemeStyle: style}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), wantTag) {
		t.Fatalf("missing tenant theme tag.\nwant fragment: %q\nrendered: %q", wantTag, buf.String())
	}
}

// TestInboxLayout_OmitsTenantThemeStyleWhenEmpty pins the {{with}} guard.
func TestInboxLayout_OmitsTenantThemeStyleWhenEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := inboxLayoutTmpl.Execute(&buf, layoutData{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(buf.String(), `id="tenant-theme"`) {
		t.Fatalf("empty TenantThemeStyle must not emit <style> tag: %q", buf.String())
	}
}
