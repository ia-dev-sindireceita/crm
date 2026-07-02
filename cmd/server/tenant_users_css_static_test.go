package main

// SIN-66499 — regression guard for the user-management stylesheet.
//
// internal/web/tenantusers/templates.go references /static/css/users.css
// (after tokens.css + components.css, injected via the shell "head_extra").
// If the file is missing on disk the link tag 404s silently and the
// /settings/users surface renders with user-agent defaults. Parallel to
// TestChannelsStylesheet_ServedAsCSS.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUsersStylesheet_ServedAsCSS(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/users.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/users.css must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("served body is empty — users.css must have rules")
	}
	for _, needle := range []string{
		".users-page",
		".users-list",
		".users-credential__value",
		"var(--text-strong",
		"var(--surface-2",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("users.css missing required selector or token reference %q", needle)
		}
	}
	// Tokens-only contract: no raw hex colours in the surface sheet.
	if strings.Contains(body, "#") {
		t.Errorf("users.css must be token-only — found a raw hex/# reference")
	}
}
