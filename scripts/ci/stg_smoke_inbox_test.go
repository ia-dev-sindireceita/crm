// Package ci_test (file: stg_smoke_inbox_test.go) drives
// scripts/ci/stg-smoke-inbox.sh against an in-process httptest server
// that mimics the staging operator-inbox surface. The test exists so a
// future edit to the smoke script does not silently shift the failure-
// mode taxonomy the cd-stg job relies on, and so the script's exit-code
// guarantees stay green under `go test ./...`.
//
// Why drive bash from Go: scripts/tests/backup_test.sh established the
// hand-rolled bash runner precedent for scripts/backup.sh, but the inbox
// smoke's surface is HTTP (cookies + JSON + HTML parsing), which a Go
// httptest is dramatically simpler to fake than a bash stub PATH. We
// keep the bash script as the production artifact (matches the existing
// cd-stg.yml /login smoke pattern) and use this Go test only as a
// behaviour spec.
//
// Test taxonomy:
//   - happy_path: every stage succeeds, inbound arrives on the second
//     poll, exit 0.
//   - preflight_provider_disabled: /health reports disabled (SIN-63858
//     fix-forward); smoke degrades — auth + /inbox 200 + exit 0.
//   - preflight_provider_missing: /health omits the field (pre-W6
//     binary); smoke degrades — same as disabled.
//   - preflight_provider_unknown: /health reports a non-enum value;
//     stage=preflight, exit 1.
//   - preflight_provider_real: /health reports the reserved-but-
//     unwired carrier slot; stage=preflight, exit 1.
//   - degraded_inbox_empty: degraded mode tolerates the empty-list
//     template (no llmcustomer bootstrap, no stage=bootstrap fail).
//   - route_404: /inbox responds 404; stage=route, exit 1.
//   - bootstrap_empty: full mode with empty list HTML;
//     stage=bootstrap, exit 1.
//   - dispatch_timeout: send succeeds but no inbound ever appears;
//     stage=dispatch, exit 1.

package ci_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// smokeScriptPath resolves scripts/ci/stg-smoke-inbox.sh relative to the
// test file. Keeping the resolution local means the test runs identically
// from `go test ./scripts/ci/...` and from the repo root.
func smokeScriptPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("./stg-smoke-inbox.sh")
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	return abs
}

// inboxFakeOptions tunes the behaviour of the in-process fake.
type inboxFakeOptions struct {
	// HealthProvider is the value rendered as inbox_channel_provider.
	// Empty string → field omitted.
	HealthProvider string
	// InboxStatus is the HTTP code returned by GET /inbox. 200 by default.
	InboxStatus int
	// InboxEmpty makes /inbox render the empty-state HTML instead of
	// the one-conversation HTML. Used to drive the bootstrap-empty
	// failure mode.
	InboxEmpty bool
	// InboundAfterPoll is the number of post-send view fetches required
	// before the fake renders the second msg-in bubble. 0 means the
	// inbound is visible immediately; a large value forces a timeout.
	InboundAfterPoll int32
	// ObservedSend, when non-nil, receives the headers the smoke POSTed
	// to /inbox/conversations/{id}/messages. Lets a dedicated test
	// assert the browser-like envelope (Origin, Referer) without
	// changing the default fake behaviour.
	ObservedSend *atomic.Pointer[sendObservation]
}

const fakeConversationID = "11111111-2222-3333-4444-555555555555"
const fakeCSRF = "fake-csrf-token-abc123"

// newInboxFake spins up an httptest.Server that implements the subset
// of routes the smoke exercises. Returns the server URL (suitable for
// STG_BASE) and a cleanup.
func newInboxFake(t *testing.T, opts inboxFakeOptions) string {
	t.Helper()
	postSendViewCount := int32(0)
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := `{"status":"ok","commit_sha":"unknown"`
		if opts.HealthProvider != "" {
			body += fmt.Sprintf(`,"inbox_channel_provider":%q`, opts.HealthProvider)
		}
		body += "}"
		_, _ = w.Write([]byte(body))
	})

	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		// Echo the cookie contract of the real handler: a __Host-* pair
		// the script greps for. Path=/ + Secure=false is acceptable
		// because httptest.NewServer serves plain HTTP and curl does
		// not enforce the __Host- name rule.
		w.Header().Add("Set-Cookie", "__Host-sess-tenant=fake-session; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", "__Host-csrf=fake-csrf-token-abc123; Path=/")
		w.Header().Set("Location", "/hello-tenant")
		w.WriteHeader(http.StatusFound)
	})

	mux.HandleFunc("/inbox", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inbox" {
			http.NotFound(w, r)
			return
		}
		if opts.InboxStatus != 0 && opts.InboxStatus != http.StatusOK {
			w.WriteHeader(opts.InboxStatus)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if opts.InboxEmpty {
			_, _ = w.Write([]byte(`<!doctype html><html><body>
<meta name="csrf-token" content="` + fakeCSRF + `">
<ul class="conversation-list"><li class="conversation-list__empty">Nenhuma conversa.</li></ul>
</body></html>`))
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><html><body>
<meta name="csrf-token" content="` + fakeCSRF + `">
<ul class="conversation-list">
<li class="conversation-list__item"><a class="conversation-list__link"
   href="/inbox/conversations/` + fakeConversationID + `?assigned=&amp;channel=&amp;state=open"
   hx-get="/inbox/conversations/` + fakeConversationID + `?assigned=&amp;channel=&amp;state=open">whatsapp</a></li>
</ul></body></html>`))
	})

	// Conversation view. The HTML the smoke parses needs the hidden
	// _csrf input AND a controllable number of msg-in bubbles. We track
	// view-fetch attempts via the atomic counter so the dispatch poll
	// can be driven deterministically.
	mux.HandleFunc(fmt.Sprintf("/inbox/conversations/%s", fakeConversationID),
		func(w http.ResponseWriter, _ *http.Request) {
			n := atomic.AddInt32(&postSendViewCount, 1)
			// Always start with one inbound bubble (baseline=1). After
			// InboundAfterPoll additional views, render a second msg-in
			// bubble so the poll succeeds.
			inboundBubbles := 1
			if opts.InboundAfterPoll == 0 || n > 1+opts.InboundAfterPoll {
				inboundBubbles = 2
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			body := `<article><ol id="conversation-thread">`
			for i := 0; i < inboundBubbles; i++ {
				body += "<li class=\"message-bubble msg-in\" data-status=\"read\"><p>inbound</p></li>\n"
			}
			body += `</ol><form><input type="hidden" name="_csrf" value="` + fakeCSRF + `"></form></article>`
			_, _ = w.Write([]byte(body))
		})

	// Record what the smoke sent on POST messages so dedicated tests
	// can assert the CSRF allowlist contract (Origin/Referer headers).
	// The real csrf middleware (internal/adapter/httpapi/csrf) rejects
	// with reason=csrf.origin_missing when both are absent — mirror
	// that here so a regression in the smoke (forgetting Origin) is
	// caught by `go test ./scripts/ci/...` rather than the cd-stg job.
	mux.HandleFunc(fmt.Sprintf("/inbox/conversations/%s/messages", fakeConversationID),
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "wrong method", http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get("X-CSRF-Token") == "" {
				http.Error(w, "missing csrf", http.StatusForbidden)
				return
			}
			if r.Header.Get("Origin") == "" && r.Header.Get("Referer") == "" {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			if opts.ObservedSend != nil {
				opts.ObservedSend.Store(&sendObservation{
					Origin:  r.Header.Get("Origin"),
					Referer: r.Header.Get("Referer"),
				})
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<li class="message-bubble msg-out" data-status="pending"><p>ack</p></li>`))
		})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// sendObservation captures the headers the smoke POSTed to
// /inbox/conversations/{id}/messages so a dedicated test can assert the
// browser-like envelope (Origin, Referer).
type sendObservation struct {
	Origin  string
	Referer string
}

// runSmoke invokes the smoke script with the env required by its
// preflight checks. Returns combined stdout+stderr and the exit code.
func runSmoke(t *testing.T, base string, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", smokeScriptPath(t))
	cmd.Env = append([]string{
		"PATH=" + envPath(),
		"STG_BASE=" + base,
		"STG_SEED_AGENT_EMAIL=agent@acme.test",
		"STG_SEED_AGENT_PASSWORD=stg-password",
		"POLL_TIMEOUT_SECONDS=3",
		"POLL_INTERVAL_SECONDS=1",
	}, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run smoke: %v\n%s", err, out)
	}
	return string(out), code
}

// envPath returns the minimal PATH the script needs. We inherit the
// real PATH so curl + jq + grep + sed + date resolve as in CI.
func envPath() string {
	// /usr/local/bin first so brew-installed jq on macOS dev wins; then
	// /usr/bin for the GitHub runner default.
	return "/usr/local/bin:/usr/bin:/bin"
}

func TestSmoke_HappyPath(t *testing.T) {
	t.Parallel()
	base := newInboxFake(t, inboxFakeOptions{
		HealthProvider:   "llmcustomer",
		InboundAfterPoll: 1, // inbound appears on the second post-send view
	})
	out, code := runSmoke(t, base)
	if code != 0 {
		t.Fatalf("smoke exit=%d (want 0)\n%s", code, out)
	}
	for _, want := range []string{
		"stage=preflight ok",
		"stage=auth ok",
		"stage=route ok",
		"stage=view ok",
		"stage=send ok",
		"stage=dispatch ok",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("smoke output missing %q\n%s", want, out)
		}
	}
}

func TestSmoke_PreflightProviderDisabled(t *testing.T) {
	t.Parallel()
	// SIN-63858 fix-forward: when the VPS has not enabled the fake-
	// customer adapter, the smoke must validate only auth + /inbox 200
	// and exit clean instead of false-blocking the deploy gate.
	base := newInboxFake(t, inboxFakeOptions{
		HealthProvider: "disabled",
	})
	out, code := runSmoke(t, base)
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0 (degraded mode should pass)\n%s", code, out)
	}
	for _, want := range []string{
		"stage=preflight degrade",
		"stage=auth ok",
		"stage=route ok — /inbox 200 (degraded mode)",
		"stg-smoke-inbox: PASS (degraded",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("degraded smoke output missing %q\n%s", want, out)
		}
	}
	// Dispatch stages must NOT have run.
	for _, forbid := range []string{"stage=view", "stage=send", "stage=dispatch"} {
		if strings.Contains(out, forbid) {
			t.Fatalf("degraded smoke ran a dispatch stage %q (must be skipped)\n%s", forbid, out)
		}
	}
}

func TestSmoke_PreflightProviderMissing(t *testing.T) {
	t.Parallel()
	// Field omitted from /health (pre-W6 binary). Same degrade contract
	// as `disabled` — empty value reads as not-yet-enabled.
	base := newInboxFake(t, inboxFakeOptions{
		HealthProvider: "",
	})
	out, code := runSmoke(t, base)
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0 (degraded mode should pass for missing field)\n%s", code, out)
	}
	if !strings.Contains(out, "stage=preflight degrade") {
		t.Fatalf("smoke output missing stage=preflight degrade label\n%s", out)
	}
}

func TestSmoke_PreflightProviderUnknown(t *testing.T) {
	t.Parallel()
	// A typo or future-unknown value must fail-loud, not silently
	// degrade — otherwise a misconfigured prod could green-smoke.
	base := newInboxFake(t, inboxFakeOptions{
		HealthProvider: "bogus-value",
	})
	out, code := runSmoke(t, base)
	if code == 0 {
		t.Fatalf("smoke exit=0 want non-zero for unknown provider\n%s", out)
	}
	if !strings.Contains(out, "stage=preflight") {
		t.Fatalf("smoke output missing stage=preflight failure label\n%s", out)
	}
}

func TestSmoke_PreflightProviderReal(t *testing.T) {
	t.Parallel()
	// The reserved-but-unwired carrier slot is rejected because the
	// smoke does not yet know how to exercise a real carrier loop.
	base := newInboxFake(t, inboxFakeOptions{
		HealthProvider: "real",
	})
	out, code := runSmoke(t, base)
	if code == 0 {
		t.Fatalf("smoke exit=0 want non-zero for provider=real\n%s", out)
	}
	if !strings.Contains(out, "stage=preflight") {
		t.Fatalf("smoke output missing stage=preflight failure label\n%s", out)
	}
}

func TestSmoke_DegradedAcceptsEmptyInbox(t *testing.T) {
	t.Parallel()
	// In degraded mode the empty-state template is the expected shape
	// (no llmcustomer bootstrap to seed a synthetic conversation). The
	// smoke must NOT trip stage=bootstrap.
	base := newInboxFake(t, inboxFakeOptions{
		HealthProvider: "disabled",
		InboxEmpty:     true,
	})
	out, code := runSmoke(t, base)
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0 (degraded + empty inbox must pass)\n%s", code, out)
	}
	if strings.Contains(out, "stage=bootstrap") {
		t.Fatalf("degraded smoke tripped stage=bootstrap on empty inbox (must be skipped)\n%s", out)
	}
}

func TestSmoke_RouteNotMounted(t *testing.T) {
	t.Parallel()
	base := newInboxFake(t, inboxFakeOptions{
		HealthProvider: "llmcustomer",
		InboxStatus:    http.StatusNotFound,
	})
	out, code := runSmoke(t, base)
	if code == 0 {
		t.Fatalf("smoke exit=0 want non-zero\n%s", out)
	}
	if !strings.Contains(out, "stage=route") {
		t.Fatalf("smoke output missing stage=route failure label\n%s", out)
	}
}

func TestSmoke_BootstrapEmpty(t *testing.T) {
	t.Parallel()
	base := newInboxFake(t, inboxFakeOptions{
		HealthProvider: "llmcustomer",
		InboxEmpty:     true,
	})
	out, code := runSmoke(t, base)
	if code == 0 {
		t.Fatalf("smoke exit=0 want non-zero\n%s", out)
	}
	if !strings.Contains(out, "stage=bootstrap") {
		t.Fatalf("smoke output missing stage=bootstrap failure label\n%s", out)
	}
}

// TestSmoke_Auth_LowercaseSetCookie regression-locks SIN-63858 cd-stg
// run 26724483191: the staging edge is HTTP/2, so `set-cookie:` arrives
// lowercase. The script's auth-stage greps must be case-insensitive, or
// every full-mode deploy false-fails with `missing __Host-sess-tenant`
// even though login succeeded and the cookies are present. The default
// httptest server normalizes headers to canonical case, hiding the bug;
// this test hijacks the connection and writes the response bytes by
// hand so the failure mode is faithful to staging.
func TestSmoke_Auth_LowercaseSetCookie(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","inbox_channel_provider":"disabled"}`))
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter is not a Hijacker")
		}
		conn, bw, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		defer conn.Close()
		// Write the response with lowercase header names to mimic HTTP/2
		// on the wire (curl --http2 prints headers in lowercase even on
		// the -D dump). HTTP/1.1 protocol-level grammar tolerates
		// case-insensitive header names, so this is a valid response.
		_, _ = bw.WriteString(strings.Join([]string{
			"HTTP/1.1 302 Found",
			"location: /hello-tenant",
			"set-cookie: __Host-sess-tenant=fake-session; Path=/; HttpOnly",
			"set-cookie: __Host-csrf=fake-csrf-token-abc123; Path=/",
			"content-length: 0",
			"",
			"",
		}, "\r\n"))
		_ = bw.Flush()
	})
	mux.HandleFunc("/inbox", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body>
<ul class="conversation-list"><li class="conversation-list__empty">Nenhuma conversa.</li></ul>
</body></html>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	out, code := runSmoke(t, srv.URL)
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0 (lowercase set-cookie must be tolerated)\n%s", code, out)
	}
	if !strings.Contains(out, "stage=auth ok") {
		t.Fatalf("smoke output missing stage=auth ok on lowercase set-cookie response\n%s", out)
	}
}

func TestSmoke_DispatchTimeout(t *testing.T) {
	t.Parallel()
	base := newInboxFake(t, inboxFakeOptions{
		HealthProvider:   "llmcustomer",
		InboundAfterPoll: 999, // never grows past baseline within budget
	})
	out, code := runSmoke(t, base, "POLL_TIMEOUT_SECONDS=2", "POLL_INTERVAL_SECONDS=1")
	if code == 0 {
		t.Fatalf("smoke exit=0 want non-zero\n%s", out)
	}
	if !strings.Contains(out, "stage=dispatch") {
		t.Fatalf("smoke output missing stage=dispatch failure label\n%s", out)
	}
}

// TestSmoke_DispatchSoftCheckByDefault pins SIN-65409 Opção A: when the
// LLM round-trip never completes (e.g. OpenRouter key/credit/latency on
// the VPS), the default deploy is NOT failed. stage=dispatch is a
// warn-only soft-check — it emits a ::warning:: annotation and exits 0
// so a correct code deploy ships despite a paid external dependency
// being unavailable. The auto-reply signal stays observable via the
// per-attempt `inbound=` logs and the warning. The earlier gates
// (auth/route/view/send) still run and would have blocked on failure.
func TestSmoke_DispatchSoftCheckByDefault(t *testing.T) {
	t.Parallel()
	base := newInboxFake(t, inboxFakeOptions{
		HealthProvider:   "llmcustomer",
		InboundAfterPoll: 999, // inbound never grows past baseline within budget
	})
	// No DISPATCH_BLOCKING in the env → default false → soft-check.
	out, code := runSmoke(t, base, "POLL_TIMEOUT_SECONDS=2", "POLL_INTERVAL_SECONDS=1")
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0 (dispatch soft-check must not fail the deploy)\n%s", code, out)
	}
	// The warning annotation must be emitted so the signal stays
	// observable in the cd-stg job log.
	if !strings.Contains(out, "::warning::stage=dispatch") {
		t.Fatalf("smoke output missing ::warning::stage=dispatch annotation\n%s", out)
	}
	if !strings.Contains(out, "DISPATCH_BLOCKING=true") {
		t.Fatalf("soft-check output should document the DISPATCH_BLOCKING re-arm escape hatch\n%s", out)
	}
	// The earlier blocking gates must still have run (regression guard:
	// soft-check only relaxes the final stage, not the incident coverage).
	for _, want := range []string{"stage=auth ok", "stage=route ok", "stage=view ok", "stage=send ok"} {
		if !strings.Contains(out, want) {
			t.Fatalf("soft-check smoke skipped a blocking gate, missing %q\n%s", want, out)
		}
	}
	// A blocking ::error:: must NOT be emitted on the default path.
	if strings.Contains(out, "::error::stage=dispatch") {
		t.Fatalf("default dispatch path emitted a blocking ::error:: (must be warn-only)\n%s", out)
	}
}

// TestSmoke_DispatchBlockingReArm pins the reversibility AC: an operator
// can re-arm stage=dispatch as a hard deploy gate with
// DISPATCH_BLOCKING=true. With the flag set, a dispatch timeout fails
// the smoke (exit 1, ::error::) exactly as it did before SIN-65409.
func TestSmoke_DispatchBlockingReArm(t *testing.T) {
	t.Parallel()
	base := newInboxFake(t, inboxFakeOptions{
		HealthProvider:   "llmcustomer",
		InboundAfterPoll: 999, // never grows past baseline within budget
	})
	out, code := runSmoke(t, base,
		"POLL_TIMEOUT_SECONDS=2", "POLL_INTERVAL_SECONDS=1", "DISPATCH_BLOCKING=true")
	if code == 0 {
		t.Fatalf("smoke exit=0 want non-zero (DISPATCH_BLOCKING=true must re-arm the gate)\n%s", out)
	}
	if !strings.Contains(out, "::error::stage=dispatch") {
		t.Fatalf("re-armed dispatch gate must emit ::error::stage=dispatch\n%s", out)
	}
}

// TestSmoke_Send_CSRFOriginEnvelope pins the contract that prevented
// PR #131 from green-deploying: the smoke MUST send Origin and Referer
// on the POST so the production csrf middleware accepts the request.
// Without these headers the real handler rejects with
// reason=csrf.origin_missing (internal/adapter/httpapi/csrf
// readOriginOrReferer) and the smoke false-fails as "stage=send: 403"
// even though the operator's authorization is correct.
//
// The fake's send handler returns 403 when both headers are absent, so
// the test would also catch a regression where a future edit drops the
// headers from the curl invocation.
func TestSmoke_Send_CSRFOriginEnvelope(t *testing.T) {
	t.Parallel()
	var observed atomic.Pointer[sendObservation]
	base := newInboxFake(t, inboxFakeOptions{
		HealthProvider:   "llmcustomer",
		InboundAfterPoll: 1,
		ObservedSend:     &observed,
	})
	out, code := runSmoke(t, base)
	if code != 0 {
		t.Fatalf("smoke exit=%d (want 0)\n%s", code, out)
	}
	got := observed.Load()
	if got == nil {
		t.Fatalf("smoke did not POST to /messages with CSRF-allowlist headers\n%s", out)
	}
	if got.Origin != base {
		t.Fatalf("Origin = %q, want %q (must match STG_BASE so csrf allowlist accepts the request)", got.Origin, base)
	}
	wantRefererPrefix := base + "/inbox/conversations/"
	if !strings.HasPrefix(got.Referer, wantRefererPrefix) {
		t.Fatalf("Referer = %q, want prefix %q", got.Referer, wantRefererPrefix)
	}
}
