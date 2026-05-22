package branding

import (
	"bytes"
	"strings"
	"testing"

	domain "github.com/pericles-luz/crm/internal/branding"
)

// TestSaveResponse_StampCSPNonce pins SIN-63275 on the OOB-swap
// fragment that refreshes <style id="tenant-theme"> after POST
// /branding/save. The fragment carries `nonce="{{.CSPNonce}}"` so the
// strict `style-src 'self' 'nonce-…'` policy accepts the inline tag
// once HTMX swaps it into the live document. Without the nonce the
// browser silently drops the swapped stylesheet and the user sees the
// stale palette until full reload.
func TestSaveResponse_StampCSPNonce(t *testing.T) {
	t.Parallel()
	style := domain.DefaultThemeStyle
	const nonce = "test-csp-nonce-branding"
	wantTag := `<style id="tenant-theme" nonce="` + nonce + `" hx-swap-oob="outerHTML">` + string(style) + `</style>`

	var buf bytes.Buffer
	if err := saveTmpl.Execute(&buf, saveData{
		ThemeStyle: style,
		Message:    "ok",
		CSPNonce:   nonce,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), wantTag) {
		t.Fatalf("missing nonced OOB style.\nwant fragment: %q\nrendered: %q", wantTag, buf.String())
	}
}

// TestSaveResponse_FailClosedWhenNonceEmpty pins the fail-closed
// path: when CSPNonce is empty (middleware absent), the fragment
// still emits `nonce=""` so the browser blocks the inline tag rather
// than silently allowing it.
func TestSaveResponse_FailClosedWhenNonceEmpty(t *testing.T) {
	t.Parallel()
	style := domain.DefaultThemeStyle
	wantTag := `<style id="tenant-theme" nonce="" hx-swap-oob="outerHTML">` + string(style) + `</style>`

	var buf bytes.Buffer
	if err := saveTmpl.Execute(&buf, saveData{
		ThemeStyle: style,
		Message:    "ok",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(buf.String(), wantTag) {
		t.Fatalf("missing fail-closed OOB style.\nwant fragment: %q\nrendered: %q", wantTag, buf.String())
	}
}
