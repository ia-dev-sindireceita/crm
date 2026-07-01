package main

// SIN-66391 (P2) — regression guard for the channel-management stylesheet.
//
// internal/web/channels/templates.go references /static/css/channels.css
// (after tokens.css + components.css, injected via the shell "head_extra").
// If the file is missing on disk the link tag 404s silently and the
// /settings/channels surface renders with user-agent defaults. Spinning up
// the same FileServer setup main.go mounts in production proves the asset
// exists and is served as text/css. Parallel to
// TestDashboardStylesheet_ServedAsCSS (dashboard_css_static_test.go).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChannelsStylesheet_ServedAsCSS(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/channels.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/channels.css must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("served body is empty — channels.css must have rules")
	}
	// Spot-check the load-bearing selectors + the tokens-only contract so a
	// future refactor that hard-codes a raw colour or drops the roster
	// primitive regresses here instead of in staging.
	for _, needle := range []string{
		".channels-page",
		".channels-list",
		".channels-roster",
		".channels-roster__row",
		"var(--border-subtle",
		"var(--text-strong",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("channels.css missing required selector or token reference %q", needle)
		}
	}
	// Tokens-only contract: no raw hex colours in the surface sheet.
	if strings.Contains(body, "#") {
		t.Errorf("channels.css must be token-only — found a raw hex/# reference")
	}
}
