// Package ci_test (file: stg_smoke_inbox_session_reuse_test.go) proves
// the SIN-65377 session-reuse contract of scripts/ci/stg-smoke-inbox.sh.
//
// Root cause (cd-stg run 27879210465): the deploy smoke steps all run
// from the same runner IP, and POST /login is rate-limited per IP at
// {Window 1min, Max 5} (internal/iam/ratelimit/policy.go). The /login
// smoke and the /inbox smoke each re-authenticated, so the cumulative
// POST /login count tripped the limiter — the /inbox step 429'd and the
// /master/tenants step was skipped, turning a successful deploy red.
//
// The fix lets the /login smoke export its session cookie jar
// (STG_SESSION_JAR) so the /inbox smoke reuses it instead of logging in
// again. These tests are the "dry-run proving the sequence does not
// blow the bucket" required by the issue: with the jar provided the
// script makes ZERO POST /login, and the fallback login still works
// when the jar is absent or stale.
package ci_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// reuseFake stands up an inbox surface that succeeds end-to-end and
// counts every POST /login so a test can assert how many the script
// spent. loginStatus controls what /login returns: a healthy 302 for
// the fallback path, or 500 in the reuse path where /login must never
// be called at all (a non-zero count then fails loudly).
func reuseFake(t *testing.T, loginStatus int, loginHits *atomic.Int32) string {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","commit_sha":"unknown","inbox_channel_provider":"llmcustomer"}`))
	})

	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		loginHits.Add(1)
		if loginStatus != http.StatusFound {
			http.Error(w, "login should not have been called", loginStatus)
			return
		}
		w.Header().Add("Set-Cookie", "__Host-sess-tenant=fake-session; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", "__Host-csrf="+fakeCSRF+"; Path=/")
		w.Header().Set("Location", "/hello-tenant")
		w.WriteHeader(http.StatusFound)
	})

	mux.HandleFunc("/inbox", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inbox" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body>
<ul class="conversation-list">
<li class="conversation-list__item"><a class="conversation-list__link"
   href="/inbox/conversations/` + fakeConversationID + `?assigned=&amp;channel=&amp;state=open">whatsapp</a></li>
</ul></body></html>`))
	})

	var viewCount atomic.Int32
	mux.HandleFunc(fmt.Sprintf("/inbox/conversations/%s", fakeConversationID),
		func(w http.ResponseWriter, _ *http.Request) {
			// baseline=1 on the first view, then grow to 2 so the
			// dispatch poll observes an inbound and exits 0 quickly.
			n := viewCount.Add(1)
			bubbles := 1
			if n > 1 {
				bubbles = 2
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			body := `<article><ol id="conversation-thread">`
			for i := 0; i < bubbles; i++ {
				body += "<li class=\"message-bubble msg-in\" data-status=\"read\"><p>in</p></li>\n"
			}
			body += `</ol><form><input type="hidden" name="_csrf" value="` + fakeCSRF + `"></form></article>`
			_, _ = w.Write([]byte(body))
		})

	mux.HandleFunc(fmt.Sprintf("/inbox/conversations/%s/messages", fakeConversationID),
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.Header.Get("X-CSRF-Token") == "" {
				http.Error(w, "bad send", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<li class="message-bubble msg-out" data-status="pending"><p>ack</p></li>`))
		})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// writeSessionJar creates a Netscape-format cookie jar carrying the
// named cookies and returns its path. Mirrors what `curl -c` persists
// after a successful login.
func writeSessionJar(t *testing.T, cookies map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jar")
	body := "# Netscape HTTP Cookie File\n"
	for name, val := range cookies {
		// domain \t includeSubdomains \t path \t secure \t expiry \t name \t value
		body += fmt.Sprintf("127.0.0.1\tFALSE\t/\tFALSE\t0\t%s\t%s\n", name, val)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write jar: %v", err)
	}
	return path
}

// TestSmoke_SessionReuse_SkipsLogin is the core SIN-65377 proof: when a
// valid session jar is supplied the script makes ZERO POST /login and
// still drives the full inbox loop. /login is wired to 500 so any
// accidental call fails the test loudly.
func TestSmoke_SessionReuse_SkipsLogin(t *testing.T) {
	t.Parallel()
	var loginHits atomic.Int32
	base := reuseFake(t, http.StatusInternalServerError, &loginHits)
	jar := writeSessionJar(t, map[string]string{
		"__Host-sess-tenant": "fake-session",
		"__Host-csrf":        fakeCSRF,
	})

	out, code := runSmoke(t, base, "STG_SESSION_JAR="+jar)
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0 (reuse path should pass)\n%s", code, out)
	}
	if got := loginHits.Load(); got != 0 {
		t.Fatalf("POST /login count=%d want 0 — session jar reuse must NOT re-authenticate (SIN-65377)\n%s", got, out)
	}
	for _, want := range []string{"stage=auth reuse", "stage=auth ok", "stage=dispatch ok"} {
		if !strings.Contains(out, want) {
			t.Fatalf("reuse smoke output missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "stage=auth POST") {
		t.Fatalf("reuse smoke still POSTed /login (saw 'stage=auth POST')\n%s", out)
	}
}

// TestSmoke_SessionReuse_FallbackWhenJarMissingCookie proves the
// fallback: a jar that does NOT carry __Host-sess-tenant (e.g. an
// upstream step that failed to mint a session) must not be trusted —
// the script logs in exactly once instead.
func TestSmoke_SessionReuse_FallbackWhenJarMissingCookie(t *testing.T) {
	t.Parallel()
	var loginHits atomic.Int32
	base := reuseFake(t, http.StatusFound, &loginHits)
	// Jar present but only carries an unrelated cookie → reuse guard
	// fails, fallback login runs.
	jar := writeSessionJar(t, map[string]string{"__Host-csrf": fakeCSRF})

	out, code := runSmoke(t, base, "STG_SESSION_JAR="+jar)
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0 (fallback path should pass)\n%s", code, out)
	}
	if got := loginHits.Load(); got != 1 {
		t.Fatalf("POST /login count=%d want 1 — stale jar must fall back to a single login\n%s", got, out)
	}
	if !strings.Contains(out, "stage=auth POST") {
		t.Fatalf("fallback smoke did not POST /login (missing 'stage=auth POST')\n%s", out)
	}
	if strings.Contains(out, "stage=auth reuse") {
		t.Fatalf("fallback smoke wrongly took the reuse path on a stale jar\n%s", out)
	}
}

// TestSmoke_SessionReuse_FallbackWhenJarUnset proves the unset-env path
// is unchanged: no STG_SESSION_JAR → exactly one login.
func TestSmoke_SessionReuse_FallbackWhenJarUnset(t *testing.T) {
	t.Parallel()
	var loginHits atomic.Int32
	base := reuseFake(t, http.StatusFound, &loginHits)

	out, code := runSmoke(t, base)
	if code != 0 {
		t.Fatalf("smoke exit=%d want 0\n%s", code, out)
	}
	if got := loginHits.Load(); got != 1 {
		t.Fatalf("POST /login count=%d want 1 (no jar → single login)\n%s", got, out)
	}
}
