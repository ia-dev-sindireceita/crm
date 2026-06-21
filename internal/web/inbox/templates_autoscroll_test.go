package inbox

import (
	"bytes"
	"strings"
	"testing"
)

// TestInboxLayout_WiresAutoScrollScript pins SIN-65454: the full-page inbox
// layout references the external auto-scroll helper /static/js/inbox.js with
// the per-request CSP nonce. The strict script-src policy bans inline
// handlers, so the scroll-to-latest logic must load from this nonce'd
// external file — if the <script> tag is dropped, the thread silently stops
// following new messages without breaking any other test.
func TestInboxLayout_WiresAutoScrollScript(t *testing.T) {
	t.Parallel()
	const nonce = "test-csp-nonce-autoscroll"
	var buf bytes.Buffer
	if err := inboxLayoutTmpl.Execute(&buf, layoutData{CSPNonce: nonce}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	wantScript := `<script src="/static/js/inbox.js" nonce="` + nonce + `" defer></script>`
	if !strings.Contains(buf.String(), wantScript) {
		t.Fatalf("missing inbox.js auto-scroll script with nonce.\nwant fragment: %q\nrendered: %q", wantScript, buf.String())
	}
}
