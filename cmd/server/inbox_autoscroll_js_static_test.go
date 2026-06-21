package main

// SIN-65454 — regression guard for the inbox auto-scroll helper. The inbox
// shell layout (internal/web/inbox/templates.go head_extra) references
// /static/js/inbox.js with a CSP nonce; if the file is missing on disk the
// <script> tag 404s under strict CSP and the conversation thread stops
// auto-scrolling to the latest message without breaking a single Go test.
// Mirrors TestImpersonationCountdownScript_ServedAsJS.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInboxAutoScrollScript_ServedAsJS(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/js/inbox.js", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — /static/js/inbox.js missing on disk", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "javascript")
	}

	body := rec.Body.String()
	if len(body) < 200 {
		t.Fatalf("body too short (%d bytes) — inbox.js stub or empty?", len(body))
	}

	// Pin one needle per behaviour the script must wire so a future refactor
	// that drops the swap-event binding or the pin-to-bottom heuristic fails
	// this guard rather than silently breaking auto-scroll.
	for _, needle := range []string{
		`conversation-thread`,
		`thread-live-poll`,
		`htmx:beforeSwap`,
		`htmx:afterSwap`,
		`scrollHeight`,
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("inbox.js missing %q — auto-scroll wiring incomplete", needle)
		}
	}
}
