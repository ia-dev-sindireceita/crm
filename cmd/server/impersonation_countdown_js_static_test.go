package main

// SIN-65379 / UX-F10 — regression guard for the impersonation
// countdown script. The shell layout (internal/web/shell/layout.html)
// and every master full-page layout (internal/web/master/*.go)
// reference /static/js/impersonation-countdown.js; if the file is
// missing on disk the <script> tag 404s under strict CSP and the
// countdown pill stays blank without breaking a single Go test.
// Mirrors TestMasterGrantsToggleScript_ServedAsJS.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImpersonationCountdownScript_ServedAsJS(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/js/impersonation-countdown.js", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — /static/js/impersonation-countdown.js missing on disk", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "javascript")
	}

	body := rec.Body.String()
	if len(body) < 200 {
		t.Fatalf("body too short (%d bytes) — impersonation-countdown.js stub or empty?", len(body))
	}

	// Pin one needle per behaviour the script must wire so a future
	// refactor that drops the data-attribute selectors or the tick loop
	// fails this guard rather than silently breaking the countdown.
	for _, needle := range []string{
		`data-impersonation-banner`,
		`data-impersonation-countdown`,
		`data-expires-at`,
		`data-server-now`,
		`setInterval`,
		`clearInterval`,
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("impersonation-countdown.js missing %q — countdown wiring incomplete", needle)
		}
	}
}
