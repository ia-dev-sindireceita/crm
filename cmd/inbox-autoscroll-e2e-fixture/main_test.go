package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	vendorintegrity "github.com/pericles-luz/crm/internal/web/vendor"
)

// TestBuildMux_HostPageWiresInboxAssets verifies the fixture host page
// links the production inbox assets exactly as inboxLayoutTmpl does: the
// htmx-config meta, inbox.css, the vendored htmx bundle, and inbox.js as a
// nonce'd, defer'd external script. Without these the browser probe would
// not be exercising the real auto-scroll script under the real CSP.
func TestBuildMux_HostPageWiresInboxAssets(t *testing.T) {
	t.Parallel()
	mux := buildMux("../../web/static", 25)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`href="/static/css/inbox.css"`,
		`href="/fixture.css"`,
		`src="/static/vendor/htmx/2.0.9/htmx.min.js"`,
		`src="/static/js/inbox.js"`,
		// inbox.js must be the external, defer'd script — never inline.
		` defer></script>`,
		`includeIndicatorStyles`,
		`id="inbox-conversation-pane"`,
		// The open-conversation control does an innerHTML swap of the pane,
		// which is exactly what inbox.js treats as a fresh view.
		`hx-target="#inbox-conversation-pane"`,
		`hx-swap="innerHTML"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("host page missing %q\nbody: %s", want, body)
		}
	}
	// inbox.js carries behaviour; the host page itself must own no inline
	// <script> body (strict-CSP contract — inbox.js is external).
	if strings.Contains(body, "<script>") {
		t.Errorf("host page must not contain an inline <script> block\nbody: %s", body)
	}
}

// TestBuildMux_ConversationFragmentCarriesThreadContract verifies the
// conversation fragment swapped into #inbox-conversation-pane reproduces
// the production thread contract inbox.js depends on: the
// #conversation-thread scroll container, the #thread-live-poll sentinel
// carrying the inbound trigger (hx-get="/inbound" hx-trigger="click"
// hx-target="this" hx-swap="outerHTML" — so the trigger element id stays
// "thread-live-poll", exactly how the shipped inbox.js detects inbound),
// and the compose form posting to .../messages with beforeend. If any of
// these ids/attrs drift, the probe stops exercising the real selectors.
func TestBuildMux_ConversationFragmentCarriesThreadContract(t *testing.T) {
	t.Parallel()
	mux := buildMux("../../web/static", 25)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/conversation", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="conversation-thread"`,
		`class="conversation__thread"`,
		`id="thread-live-poll"`,
		`hx-post="/inbox/conversations/conv-e2e/messages"`,
		`hx-target="#conversation-thread"`,
		`hx-swap="beforeend"`,
		// The inbound trigger lives on the sentinel itself so elt.id ===
		// "thread-live-poll" matches the shipped inbox.js detection.
		`hx-get="/inbound" hx-trigger="click" hx-target="this" hx-swap="outerHTML"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("conversation fragment missing %q\nbody: %s", want, body)
		}
	}
}

// TestBuildMux_SeedThreadOverflows verifies the conversation fragment
// seeds enough bubbles for the fixed-height viewport to overflow — the
// precondition for every scroll assertion in the probe.
func TestBuildMux_SeedThreadOverflows(t *testing.T) {
	t.Parallel()
	mux := buildMux("../../web/static", 25)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/conversation", nil))

	got := strings.Count(rec.Body.String(), `class="message-bubble`)
	if got < 25 {
		t.Fatalf("seed bubble count = %d, want >= 25 so the thread overflows", got)
	}
}

// TestBuildMux_SendAppendsOutboundBubble verifies POST .../messages
// returns a single outbound bubble with a stable #msg-* anchor so the
// beforeend append lands one new bubble at the bottom of the thread.
func TestBuildMux_SendAppendsOutboundBubble(t *testing.T) {
	t.Parallel()
	mux := buildMux("../../web/static", 25)

	form := url.Values{"body": {"olá"}}
	req := httptest.NewRequest(http.MethodPost, "/inbox/conversations/conv-e2e/messages", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if c := strings.Count(body, "<li "); c != 1 {
		t.Fatalf("send response <li> count = %d, want exactly 1 bubble", c)
	}
	for _, want := range []string{`id="msg-f`, `message-bubble--out`, `data-direction="out"`, `olá`} {
		if !strings.Contains(body, want) {
			t.Errorf("send response missing %q\nbody: %s", want, body)
		}
	}
}

// TestBuildMux_SendEchoesDefaultWhenBodyEmpty guards the empty-body branch
// so an empty compose submit still appends a bubble (the probe's send step
// must always land something at the bottom).
func TestBuildMux_SendEchoesDefaultWhenBodyEmpty(t *testing.T) {
	t.Parallel()
	mux := buildMux("../../web/static", 25)

	req := httptest.NewRequest(http.MethodPost, "/inbox/conversations/conv-e2e/messages", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "mensagem enviada") {
		t.Errorf("empty-body send missing default text\nbody: %s", rec.Body.String())
	}
}

// TestBuildMux_InboundUsesOOBBeforeendShape verifies GET /inbound returns
// the SIN-65419 live-thread-update shape: a fresh #thread-live-poll
// sentinel followed by an <ol hx-swap-oob="beforeend:#conversation-thread">
// carrying one inbound bubble. inbox.js's inbound (pin-when-near-bottom)
// branch is driven precisely by this OOB append.
func TestBuildMux_InboundUsesOOBBeforeendShape(t *testing.T) {
	t.Parallel()
	mux := buildMux("../../web/static", 25)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/inbound", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="thread-live-poll"`,
		// The fresh sentinel re-arms with the same click trigger so a repeat
		// inbound fires, mirroring production re-arming the every-3s sentinel.
		`hx-get="/inbound" hx-trigger="click" hx-target="this" hx-swap="outerHTML"`,
		`hx-swap-oob="beforeend:#conversation-thread"`,
		`data-direction="in"`,
		`message-bubble--in`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("inbound response missing %q\nbody: %s", want, body)
		}
	}
}

// TestBuildMux_FixtureCSSPinsThreadHeight verifies the fixture stylesheet
// pins #conversation-thread to a fixed-height scroll viewport. Without the
// height the seed bubbles would not overflow and the scroll assertions
// would be vacuous.
func TestBuildMux_FixtureCSSPinsThreadHeight(t *testing.T) {
	t.Parallel()
	mux := buildMux("../../web/static", 25)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fixture.css", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("Content-Type = %q, want text/css prefix", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"#conversation-thread", "height: 300px", "overflow-y: auto"} {
		if !strings.Contains(body, want) {
			t.Errorf("fixture.css missing %q", want)
		}
	}
}

// TestBuildHandler_EmitsProductionCSP is the strict-CSP regression bar:
// every fixture response must carry the production Content-Security-Policy
// (script-src 'self' 'nonce-{N}', no 'unsafe-inline'). inbox.js only works
// under this policy because it is external + event-driven; mounting the
// real CSP here means an inline/eval regression fails the probe.
func TestBuildHandler_EmitsProductionCSP(t *testing.T) {
	t.Parallel()
	handler := buildHandler("../../web/static", 25)

	cases := []struct {
		name string
		req  *http.Request
	}{
		{"host_page", httptest.NewRequest(http.MethodGet, "/", nil)},
		{"conversation", httptest.NewRequest(http.MethodGet, "/conversation", nil)},
		{"inbound", httptest.NewRequest(http.MethodGet, "/inbound", nil)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, tc.req)
			hdr := rec.Header().Get("Content-Security-Policy")
			if hdr == "" {
				t.Fatalf("missing Content-Security-Policy header")
			}
			if !strings.Contains(hdr, "script-src 'self' 'nonce-") {
				t.Errorf("CSP missing nonce'd script-src\nheader: %s", hdr)
			}
			if strings.Contains(hdr, "'unsafe-inline'") {
				t.Errorf("CSP must not contain 'unsafe-inline'\nheader: %s", hdr)
			}
		})
	}
}

// TestBuildHandler_HostPageScriptCarriesCSPNonce verifies the inbox.js
// external script tag is stamped with the per-request CSP nonce so the
// browser executes it under the production policy. inbox.js is loaded with
// nonce+defer in production (inboxLayoutTmpl); the fixture must match or
// the browser would drop the script and the thread would not auto-scroll.
func TestBuildHandler_HostPageScriptCarriesCSPNonce(t *testing.T) {
	t.Parallel()
	handler := buildHandler("../../web/static", 25)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	nonce := extractNonce(rec.Header().Get("Content-Security-Policy"))
	if nonce == "" {
		t.Fatalf("CSP header missing script-src nonce")
	}
	want := `src="/static/js/inbox.js" nonce="` + nonce + `" defer>`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("inbox.js script tag missing matching nonce attribute %q\nbody: %s", want, rec.Body.String())
	}
}

// TestHostPage_SRIAttributeOnVendorScript verifies the vendored htmx
// <script> carries the SRI attribute pair (SIN-62535) so the browser
// re-verifies the bytes it executes.
func TestHostPage_SRIAttributeOnVendorScript(t *testing.T) {
	t.Parallel()
	handler := buildHandler("../../web/static", 25)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	if !strings.Contains(body, `integrity="sha384-`) {
		t.Errorf("vendored htmx <script> missing integrity=sha384-\nbody: %s", body)
	}
	if !strings.Contains(body, `crossorigin="anonymous"`) {
		t.Errorf("vendored htmx <script> missing crossorigin=anonymous\nbody: %s", body)
	}
}

// TestSeedBubbles_AlternatesDirectionAndFloorsAtOne guards the seed
// helper: at least one bubble even when asked for zero, and a mix of in/out
// directions so the visual evidence shows both bubble styles.
func TestSeedBubbles_AlternatesDirectionAndFloorsAtOne(t *testing.T) {
	t.Parallel()
	if got := seedBubbles(0); len(got) != 1 {
		t.Fatalf("seedBubbles(0) len = %d, want 1 (floor)", len(got))
	}
	got := seedBubbles(4)
	if len(got) != 4 {
		t.Fatalf("seedBubbles(4) len = %d, want 4", len(got))
	}
	var in, out int
	for _, b := range got {
		switch b.Direction {
		case "in":
			in++
		case "out":
			out++
		default:
			t.Fatalf("unexpected direction %q", b.Direction)
		}
	}
	if in == 0 || out == 0 {
		t.Fatalf("seed bubbles not mixed: in=%d out=%d", in, out)
	}
}

// TestNextID_IsUniqueAndAnchorSafe verifies appended bubbles get distinct
// ids so #msg-* anchors never collide during a probe run.
func TestNextID_IsUniqueAndAnchorSafe(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := nextID()
		if id == "" {
			t.Fatalf("nextID returned empty")
		}
		if seen[id] {
			t.Fatalf("nextID collision on %q", id)
		}
		seen[id] = true
	}
}

// TestBubbleDirClass maps direction to the production bubble modifier.
func TestBubbleDirClass(t *testing.T) {
	t.Parallel()
	if got := (bubble{Direction: "out"}).DirClass(); got != "message-bubble--out" {
		t.Errorf("out DirClass = %q", got)
	}
	if got := (bubble{Direction: "in"}).DirClass(); got != "message-bubble--in" {
		t.Errorf("in DirClass = %q", got)
	}
}

// TestMustBuildHostPageTmpl_PanicsOnUnknownAsset verifies the SIN-62535
// startup contract: an unknown vendored relpath panics during init rather
// than rendering an unverified <script> at first request.
func TestMustBuildHostPageTmpl_PanicsOnUnknownAsset(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic on missing asset, got none")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "missing required asset") {
			t.Fatalf("panic = %#v, want 'missing required asset' string", r)
		}
	}()
	mustBuildHostPageTmpl(func() (vendorintegrity.VendorIntegrity, error) {
		return emptyVendorIntegrity{}, nil
	})
}

// TestMustBuildHostPageTmpl_PanicsOnProviderError covers the provider-boot
// failure mode: the fixture must refuse to start.
func TestMustBuildHostPageTmpl_PanicsOnProviderError(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic on provider error, got none")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "load vendor integrity") {
			t.Fatalf("panic = %#v, want 'load vendor integrity' string", r)
		}
	}()
	mustBuildHostPageTmpl(func() (vendorintegrity.VendorIntegrity, error) {
		return nil, errors.New("test stub: provider boot failure")
	})
}

// TestRun_StartsServerAndShutsDownCleanlyOnContextCancel exercises the
// real bind / serve / context-cancel / shutdown loop.
func TestRun_StartsServerAndShutsDownCleanlyOnContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := pickFreeAddr(t)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-addr", addr, "-static", "../../web/static"})
	}()

	if err := waitForListener(addr, 2*time.Second); err != nil {
		t.Fatalf("server never came up: %v", err)
	}
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET / err = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("run did not return after context cancel")
	}
}

// TestRun_ReturnsErrorWhenAddrAlreadyInUse exercises the bind-error path.
func TestRun_ReturnsErrorWhenAddrAlreadyInUse(t *testing.T) {
	t.Parallel()
	addr := pickFreeAddr(t)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("preparing listener: %v", err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := run(ctx, []string{"-addr", listener.Addr().String(), "-static", "../../web/static"}); err == nil {
		t.Fatalf("run returned nil, want bind error")
	}
}

// TestRun_FlagParseErrorPropagates ensures a malformed flag set surfaces.
func TestRun_FlagParseErrorPropagates(t *testing.T) {
	t.Parallel()
	if err := run(context.Background(), []string{"-bogus-flag"}); err == nil {
		t.Fatalf("run returned nil, want flag parse error")
	}
}

func pickFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

func waitForListener(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("timeout waiting for listener")
}

// extractNonce pulls the per-request nonce out of the script-src directive.
func extractNonce(hdr string) string {
	const needle = "script-src 'self' 'nonce-"
	i := strings.Index(hdr, needle)
	if i < 0 {
		return ""
	}
	rest := hdr[i+len(needle):]
	end := strings.Index(rest, "'")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// emptyVendorIntegrity is a stub that refuses every lookup so the
// missing-asset panic path fires.
type emptyVendorIntegrity struct{}

func (emptyVendorIntegrity) SRIAttribute(string) (string, error) {
	return "", errors.New("test stub: no assets registered")
}
