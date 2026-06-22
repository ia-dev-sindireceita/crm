// Package ci_test (file: stg_smoke_rls_cross_tenant_test.go) drives
// scripts/ci/stg-smoke-rls-cross-tenant.sh against an in-process
// httptest server that mimics the staging operator-inbox surface. The
// test pins the smoke's failure-mode taxonomy (the cd-stg job greps the
// `stage=` labels) and its core guarantee: a cross-tenant attendant UUID
// rendered into the acme operator HTML must FAIL the deploy gate
// (SIN-65590 / SIN-65580), while a clean (in-tenant-only) surface passes.
//
// Same rationale as stg_smoke_inbox_test.go for driving bash from Go:
// the surface is HTTP (cookies + HTML), trivially faked with httptest.
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

const (
	rlsForbiddenUUID  = "00000000-0000-0000-0000-0000000e0e02" // agent@globex (cross-tenant control)
	rlsExpectUUID     = "00000000-0000-0000-0000-0000000a0e01" // agent@acme (in-tenant)
	rlsConversationID = "11111111-2222-3333-4444-555555555555"
)

// rlsFakeOptions tunes the in-process fake.
type rlsFakeOptions struct {
	// LeakForbidden renders the cross-tenant agent@globex UUID into the
	// assignment dropdown — i.e. RLS is bypassed. The smoke must fail.
	LeakForbidden bool
	// OmitExpected drops the in-tenant agent@acme UUID, simulating a
	// deploy where the interactive dropdown is not wired (read-only).
	// The smoke must still PASS (no leak) but emit the soft warning.
	OmitExpected bool
	// InboxStatus overrides GET /inbox (default 200).
	InboxStatus int
	// InboxEmpty renders the empty-state list (no conversation link).
	InboxEmpty bool
}

func rlsSmokeScriptPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("./stg-smoke-rls-cross-tenant.sh")
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	return abs
}

func newRLSFake(t *testing.T, opts rlsFakeOptions) string {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Add("Set-Cookie", "__Host-sess-tenant=fake-session; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", "__Host-csrf=fake-csrf; Path=/")
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
<ul class="conversation-list"><li class="conversation-list__empty">Nenhuma conversa.</li></ul>
</body></html>`))
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><html><body>
<ul class="conversation-list">
<li class="conversation-list__item"><a class="conversation-list__link"
   href="/inbox/conversations/` + rlsConversationID + `?assigned=&amp;channel=&amp;state=open">whatsapp</a></li>
</ul></body></html>`))
	})

	mux.HandleFunc(fmt.Sprintf("/inbox/conversations/%s", rlsConversationID),
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			// Mirror conversationAssignmentTmpl: each assignable attendant
			// is an <option value="<uuid>">…</option>.
			var opt strings.Builder
			opt.WriteString(`<section id="conversation-context-assignment"><select name="targetUserID">`)
			if !opts.OmitExpected {
				opt.WriteString(`<option value="` + rlsExpectUUID + `">Agent Acme</option>`)
			}
			if opts.LeakForbidden {
				opt.WriteString(`<option value="` + rlsForbiddenUUID + `">Agent Globex</option>`)
			}
			opt.WriteString(`</select></section>`)
			_, _ = w.Write([]byte(`<article>` + opt.String() + `</article>`))
		})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func runRLSSmoke(t *testing.T, base string, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", rlsSmokeScriptPath(t))
	cmd.Env = append([]string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
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

// Clean surface: in-tenant attendant present, no cross-tenant leak → PASS.
func TestRLSSmoke_NoLeakPasses(t *testing.T) {
	t.Parallel()
	base := newRLSFake(t, rlsFakeOptions{})
	out, code := runRLSSmoke(t, base)
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0 (no leak)\n%s", code, out)
	}
	for _, want := range []string{
		"stage=preflight ok",
		"stage=auth ok",
		"stage=route ok",
		"stage=view ok",
		"stage=assert ok",
		"stg-smoke-rls-cross-tenant: PASS",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("smoke output missing %q\n%s", want, out)
		}
	}
}

// The core guarantee: a cross-tenant UUID in the rendered HTML FAILS the
// deploy gate with the stage=assert label.
func TestRLSSmoke_CrossTenantLeakFails(t *testing.T) {
	t.Parallel()
	base := newRLSFake(t, rlsFakeOptions{LeakForbidden: true})
	out, code := runRLSSmoke(t, base)
	if code == 0 {
		t.Fatalf("smoke exit=0 want non-zero on cross-tenant leak\n%s", out)
	}
	if !strings.Contains(out, "stage=assert") {
		t.Fatalf("smoke output missing stage=assert failure label\n%s", out)
	}
	if !strings.Contains(out, rlsForbiddenUUID) {
		t.Fatalf("smoke output should name the leaked UUID\n%s", out)
	}
}

// A custom FORBIDDEN_TENANT_UUID is honoured (the default can be overridden).
func TestRLSSmoke_CustomForbiddenUUID(t *testing.T) {
	t.Parallel()
	const custom = "deadbeef-0000-0000-0000-000000000001"
	// Reuse the leak fake but forbid a UUID that is NOT present → must pass;
	// then forbid the UUID that IS present (acme) → must fail. This proves
	// the env var actually drives the assertion.
	base := newRLSFake(t, rlsFakeOptions{})
	out, code := runRLSSmoke(t, base, "FORBIDDEN_TENANT_UUID="+custom)
	if code != 0 {
		t.Fatalf("absent custom forbidden UUID should pass, exit=%d\n%s", code, out)
	}
	out, code = runRLSSmoke(t, base, "FORBIDDEN_TENANT_UUID="+rlsExpectUUID)
	if code == 0 {
		t.Fatalf("present custom forbidden UUID should fail\n%s", out)
	}
	if !strings.Contains(out, "stage=assert") {
		t.Fatalf("missing stage=assert label\n%s", out)
	}
}

// Dropdown not wired (read-only deploy): expected UUID absent, no leak →
// PASS with a soft ::warning::, not a failure.
func TestRLSSmoke_MissingExpectedWarnsButPasses(t *testing.T) {
	t.Parallel()
	base := newRLSFake(t, rlsFakeOptions{OmitExpected: true})
	out, code := runRLSSmoke(t, base)
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0 (read-only deploy is not a leak)\n%s", code, out)
	}
	if !strings.Contains(out, "::warning::") {
		t.Fatalf("expected a soft warning when in-tenant UUID absent\n%s", out)
	}
	if !strings.Contains(out, "stg-smoke-rls-cross-tenant: PASS") {
		t.Fatalf("smoke should still PASS\n%s", out)
	}
}

// Hard preconditions still fail loud.
func TestRLSSmoke_RoutePreconditions(t *testing.T) {
	t.Parallel()

	t.Run("inbox 404", func(t *testing.T) {
		t.Parallel()
		base := newRLSFake(t, rlsFakeOptions{InboxStatus: http.StatusNotFound})
		out, code := runRLSSmoke(t, base)
		if code == 0 {
			t.Fatalf("smoke exit=0 want non-zero on /inbox 404\n%s", out)
		}
		if !strings.Contains(out, "stage=route") {
			t.Fatalf("missing stage=route label\n%s", out)
		}
	})

	t.Run("empty inbox", func(t *testing.T) {
		t.Parallel()
		base := newRLSFake(t, rlsFakeOptions{InboxEmpty: true})
		out, code := runRLSSmoke(t, base)
		if code == 0 {
			t.Fatalf("smoke exit=0 want non-zero on empty inbox (cannot exercise dropdown)\n%s", out)
		}
		if !strings.Contains(out, "stage=route") {
			t.Fatalf("missing stage=route label\n%s", out)
		}
	})
}
