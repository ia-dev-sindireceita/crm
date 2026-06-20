// stg_smoke_aiassist_test.go drives scripts/ci/stg-smoke-aiassist.sh
// against an in-process httptest server that mimics the staging
// operator AI-assist surface. Like the inbox smoke test, this is a
// behaviour spec: it locks the failure-mode taxonomy + exit codes the
// cd-stg job relies on so a future edit to the bash script cannot
// silently shift them. SIN-65244.
//
// Taxonomy:
//   - full_summary_panel: button enabled, POST returns the summary
//     panel → PASS, "real LLM round-trip" log.
//   - full_graceful_banner: button enabled, POST returns a policy
//     banner → PASS (wired; precondition state).
//   - skip_provider_not_llmcustomer: /health provider != llmcustomer →
//     SKIP, exit 0, no auth/POST stages.
//   - skip_no_button: button absent (key unset) → SKIP, exit 0.
//   - skip_button_disabled: button present-but-disabled → SKIP, exit 0.
//   - skip_no_conversation: /inbox empty → SKIP, exit 0.
//   - fail_assist_5xx: POST returns 500 → stage=assist, exit 1.
//   - fail_assist_404: button enabled but POST 404 → stage=route, exit 1.

package ci_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func aiassistSmokeScriptPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("./stg-smoke-aiassist.sh")
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	return abs
}

type aiassistFakeOptions struct {
	// HealthProvider is rendered as inbox_channel_provider. Empty → omitted.
	HealthProvider string
	// InboxEmpty makes /inbox render no conversation link.
	InboxEmpty bool
	// ButtonState: "enabled", "disabled", or "absent".
	ButtonState string
	// AssistStatus is the POST /ai-assist HTTP status (default 200).
	AssistStatus int
	// AssistBody is the POST /ai-assist response body when status is 200.
	AssistBody string
}

const aiassistConversationID = "11111111-2222-3333-4444-555555555555"
const aiassistCSRF = "fake-csrf-token-abc123"

func newAIAssistFake(t *testing.T, opts aiassistFakeOptions) string {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := `{"status":"ok"`
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
		w.Header().Add("Set-Cookie", "__Host-sess-tenant=fake-session; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", "__Host-csrf="+aiassistCSRF+"; Path=/")
		w.Header().Set("Location", "/hello-tenant")
		w.WriteHeader(http.StatusFound)
	})

	mux.HandleFunc("/inbox", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inbox" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if opts.InboxEmpty {
			_, _ = w.Write([]byte(`<!doctype html><ul class="conversation-list"><li class="conversation-list__empty">Nenhuma conversa.</li></ul>`))
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><ul class="conversation-list">
<li><a href="/inbox/conversations/` + aiassistConversationID + `?state=open">whatsapp</a></li></ul>`))
	})

	mux.HandleFunc(fmt.Sprintf("/inbox/conversations/%s", aiassistConversationID),
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			var button string
			switch opts.ButtonState {
			case "absent":
				button = ""
			case "disabled":
				button = `<button id="ai-assist-button" class="ai-assist__button ai-assist__button--disabled" type="button" disabled>Resumir</button>`
			default: // enabled
				button = `<button id="ai-assist-button" class="ai-assist__button" type="submit">Resumir</button>`
			}
			body := `<article>` + button +
				`<form><input type="hidden" name="_csrf" value="` + aiassistCSRF + `"></form></article>`
			_, _ = w.Write([]byte(body))
		})

	mux.HandleFunc(fmt.Sprintf("/inbox/conversations/%s/ai-assist", aiassistConversationID),
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
			status := opts.AssistStatus
			if status == 0 {
				status = http.StatusOK
			}
			if status != http.StatusOK {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(status)
				// The policy-off path returns the graceful banner with a
				// non-200 (403) status by design; emit AssistBody so the
				// smoke can scan it. Fall back to a bare "error" body for
				// the genuine-fault cases (5xx, marker-less 403).
				body := opts.AssistBody
				if body == "" {
					body = "error"
				}
				_, _ = w.Write([]byte(body))
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			body := opts.AssistBody
			if body == "" {
				body = `<section class="ai-assist__result"><p class="ai-assist__summary">resumo</p></section>`
			}
			_, _ = w.Write([]byte(body))
		})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func runAIAssistSmoke(t *testing.T, base string, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", aiassistSmokeScriptPath(t))
	cmd.Env = append([]string{
		"PATH=" + envPath(),
		"STG_BASE=" + base,
		"STG_SEED_AGENT_EMAIL=agent@acme.test",
		"STG_SEED_AGENT_PASSWORD=stg-password",
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

func TestAIAssistSmoke_FullSummaryPanel(t *testing.T) {
	t.Parallel()
	base := newAIAssistFake(t, aiassistFakeOptions{
		HealthProvider: "llmcustomer",
		ButtonState:    "enabled",
	})
	out, code := runAIAssistSmoke(t, base)
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0\n%s", code, out)
	}
	for _, want := range []string{"stage=preflight ok", "stage=auth ok", "stage=route ok", "stage=view ok", "stage=assist ok — summary panel", "PASS (full"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q\n%s", want, out)
		}
	}
}

func TestAIAssistSmoke_FullGracefulBanner(t *testing.T) {
	t.Parallel()
	base := newAIAssistFake(t, aiassistFakeOptions{
		HealthProvider: "llmcustomer",
		ButtonState:    "enabled",
		AssistBody:     `<div class="ai-assist__banner ai-assist__banner--policy">IA desabilitada.</div>`,
	})
	out, code := runAIAssistSmoke(t, base)
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0 (graceful banner must pass)\n%s", code, out)
	}
	if !strings.Contains(out, "PASS (wired; precondition banner") {
		t.Fatalf("output missing graceful-banner PASS\n%s", out)
	}
}

func TestAIAssistSmoke_SkipProviderNotLLMCustomer(t *testing.T) {
	t.Parallel()
	base := newAIAssistFake(t, aiassistFakeOptions{HealthProvider: "disabled"})
	out, code := runAIAssistSmoke(t, base)
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0 (skip)\n%s", code, out)
	}
	if !strings.Contains(out, "SKIP") || !strings.Contains(out, "stage=preflight") {
		t.Fatalf("output missing preflight SKIP\n%s", out)
	}
	for _, forbid := range []string{"stage=auth ok", "stage=assist"} {
		if strings.Contains(out, forbid) {
			t.Fatalf("skip ran a later stage %q\n%s", forbid, out)
		}
	}
}

func TestAIAssistSmoke_SkipNoButton(t *testing.T) {
	t.Parallel()
	base := newAIAssistFake(t, aiassistFakeOptions{
		HealthProvider: "llmcustomer",
		ButtonState:    "absent",
	})
	out, code := runAIAssistSmoke(t, base)
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0 (no button → skip)\n%s", code, out)
	}
	if !strings.Contains(out, "no ai-assist button") || !strings.Contains(out, "SKIP") {
		t.Fatalf("output missing no-button SKIP\n%s", out)
	}
}

func TestAIAssistSmoke_SkipButtonDisabled(t *testing.T) {
	t.Parallel()
	base := newAIAssistFake(t, aiassistFakeOptions{
		HealthProvider: "llmcustomer",
		ButtonState:    "disabled",
	})
	out, code := runAIAssistSmoke(t, base)
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0 (disabled button → skip)\n%s", code, out)
	}
	if !strings.Contains(out, "present but disabled") || !strings.Contains(out, "SKIP") {
		t.Fatalf("output missing disabled-button SKIP\n%s", out)
	}
}

func TestAIAssistSmoke_SkipNoConversation(t *testing.T) {
	t.Parallel()
	base := newAIAssistFake(t, aiassistFakeOptions{
		HealthProvider: "llmcustomer",
		InboxEmpty:     true,
	})
	out, code := runAIAssistSmoke(t, base)
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0 (empty inbox → skip)\n%s", code, out)
	}
	if !strings.Contains(out, "no conversation seeded") || !strings.Contains(out, "SKIP") {
		t.Fatalf("output missing no-conversation SKIP\n%s", out)
	}
}

func TestAIAssistSmoke_FailAssist5xx(t *testing.T) {
	t.Parallel()
	base := newAIAssistFake(t, aiassistFakeOptions{
		HealthProvider: "llmcustomer",
		ButtonState:    "enabled",
		AssistStatus:   http.StatusInternalServerError,
	})
	out, code := runAIAssistSmoke(t, base)
	if code == 0 {
		t.Fatalf("smoke exit=0 want non-zero on 5xx\n%s", out)
	}
	if !strings.Contains(out, "stage=assist") {
		t.Fatalf("output missing stage=assist failure\n%s", out)
	}
}

func TestAIAssistSmoke_FailAssist404IsRouteGap(t *testing.T) {
	t.Parallel()
	base := newAIAssistFake(t, aiassistFakeOptions{
		HealthProvider: "llmcustomer",
		ButtonState:    "enabled",
		AssistStatus:   http.StatusNotFound,
	})
	out, code := runAIAssistSmoke(t, base)
	if code == 0 {
		t.Fatalf("smoke exit=0 want non-zero on 404\n%s", out)
	}
	if !strings.Contains(out, "stage=route") {
		t.Fatalf("output missing stage=route failure on 404\n%s", out)
	}
}

// TestAIAssistSmoke_PolicyBanner403 is the deny-by-default staging state:
// no ai_policy row → AIEnabled=false → the handler returns the policy
// banner with HTTP 403 (ai_assist.go StatusForbidden, pinned by
// ai_assist_test.go). The smoke must treat that as a graceful PASS, not a
// CSRF/authz hard-fail (SIN-65285: the original 403)→die branch
// false-failed real staging even though the deploy was wired correctly).
func TestAIAssistSmoke_PolicyBanner403(t *testing.T) {
	t.Parallel()
	base := newAIAssistFake(t, aiassistFakeOptions{
		HealthProvider: "llmcustomer",
		ButtonState:    "enabled",
		AssistStatus:   http.StatusForbidden,
		AssistBody:     `<div class="ai-assist__banner ai-assist__banner--policy">IA desabilitada neste canal.</div>`,
	})
	out, code := runAIAssistSmoke(t, base)
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0 (403 + policy banner is graceful)\n%s", code, out)
	}
	if !strings.Contains(out, "PASS (wired; precondition banner") {
		t.Fatalf("output missing graceful-banner PASS on 403\n%s", out)
	}
	if !strings.Contains(out, "on HTTP 403") {
		t.Fatalf("output should record the observed 403 status\n%s", out)
	}
}

// TestAIAssistSmoke_FailAssist403NoBanner pins that a 403 WITHOUT a known
// graceful marker is still a real CSRF/Origin/role rejection and must
// hard-fail — the fix accepts policy-off, not every 403.
func TestAIAssistSmoke_FailAssist403NoBanner(t *testing.T) {
	t.Parallel()
	base := newAIAssistFake(t, aiassistFakeOptions{
		HealthProvider: "llmcustomer",
		ButtonState:    "enabled",
		AssistStatus:   http.StatusForbidden,
		AssistBody:     `<h1>Forbidden</h1>`,
	})
	out, code := runAIAssistSmoke(t, base)
	if code == 0 {
		t.Fatalf("smoke exit=0 want non-zero on marker-less 403\n%s", out)
	}
	if !strings.Contains(out, "stage=assist: /ai-assist 403 with no graceful banner") {
		t.Fatalf("output missing marker-less 403 hard-fail\n%s", out)
	}
}
