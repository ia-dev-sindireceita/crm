package main

// SIN-63935 B-1 — regression guard for the app-shell toggle script.
// internal/web/shell/layout.html references /static/js/app-shell.js;
// if the file is missing on disk the link tag 404s and the hamburger +
// user-menu toggles become dead buttons at runtime without breaking a
// single Go test. Parallel to TestDesignSystemStylesheets_ServedAsCSS
// in design_system_css_static_test.go.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAppShellToggleScript_ServedAsJS(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/js/app-shell.js", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — /static/js/app-shell.js missing on disk", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "javascript")
	}

	body := rec.Body.String()
	if len(body) < 200 {
		t.Fatalf("body too short (%d bytes) — app-shell.js stub or empty?", len(body))
	}

	// Pin one needle per behaviour the script must wire so a future
	// refactor that drops the hamburger / user-menu / Escape close
	// branch fails this guard rather than silently breaking AC #4.
	for _, needle := range []string{
		".app-shell__hamburger",
		"app-shell-nav",
		".app-shell__user-menu-toggle",
		".app-shell__user-menu-panel",
		"data-collapsed",
		"aria-expanded",
		"hidden",
		"Escape",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("app-shell.js missing %q — toggle wiring incomplete", needle)
		}
	}
}
