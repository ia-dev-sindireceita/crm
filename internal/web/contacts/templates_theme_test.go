package contacts

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/contacts"
)

// stubPanelData returns a minimal valid panelData so the layout's
// embedded identity_panel partial executes without a nil deref.
func stubPanelData() panelData {
	return panelData{
		ContactID: uuid.New(),
		Identity:  &contacts.Identity{ID: uuid.New()},
	}
}

// TestContactLayout_RendersTenantThemeStyle pins the SIN-63092 wireup.
func TestContactLayout_RendersTenantThemeStyle(t *testing.T) {
	t.Parallel()
	style := branding.DefaultThemeStyle
	wantTag := `<style id="tenant-theme">` + string(style) + `</style>`
	var buf bytes.Buffer
	if err := contactLayoutTmpl.Execute(&buf, layoutData{
		Panel:            stubPanelData(),
		TenantThemeStyle: style,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), wantTag) {
		t.Fatalf("missing tenant theme tag.\nwant fragment: %q\nrendered: %q", wantTag, buf.String())
	}
}

// TestContactLayout_OmitsTenantThemeStyleWhenEmpty pins the {{with}} guard.
func TestContactLayout_OmitsTenantThemeStyleWhenEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := contactLayoutTmpl.Execute(&buf, layoutData{Panel: stubPanelData()}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(buf.String(), `id="tenant-theme"`) {
		t.Fatalf("empty TenantThemeStyle must not emit <style> tag: %q", buf.String())
	}
}
