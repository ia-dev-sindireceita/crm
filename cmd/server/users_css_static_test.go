package main

// SIN-66496 — regression guard for the tenant user-management stylesheet.
//
// internal/web/users/templates.go links /static/css/users.css (after
// tokens.css + components.css). If the file is missing on disk the link tag
// 404s silently and /settings/users renders with user-agent defaults. Serving
// it through the same FileServer main.go mounts in production proves the asset
// exists and is served as text/css. Parallel to
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
		".users-shell",
		".users-table",
		".users-form",
		"var(--border-subtle",
		"var(--text-strong",
		"var(--surface-1", // AA contrast measured against surface-1, not white
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
