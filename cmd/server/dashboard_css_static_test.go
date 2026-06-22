package main

// SIN-65096 (Tranche C · C1) — regression guard for the dashboard
// stylesheet.
//
// internal/web/dashboard/templates.go references /static/css/dashboard.css
// (alongside tokens.css + components.css). If the file is missing on disk
// the link tag 404s silently and the managerial dashboard renders with
// user-agent defaults — the "tela sem formatação" failure mode the Pitho
// sweep exists to prevent. Spinning up the same FileServer setup that
// main.go mounts in production proves the asset exists and is served as
// text/css through the registered static handler.
//
// Parallel to TestAuthStylesheet_ServedAsCSS (auth_css_static_test.go);
// both run in the cmd/server package because that is where the static-
// route wiring lives.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardStylesheet_ServedAsCSS(t *testing.T) {
	t.Parallel()
	// cmd/server lives two levels below the repo root, so the
	// web/static tree is at ../../web/static when go test runs from
	// the package directory.
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/dashboard.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/dashboard.css must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("served body is empty — dashboard.css must have rules")
	}
	// Spot-check the load-bearing selectors and the tokens-only contract
	// so a future refactor that drops the Pitho port — or hard-codes a
	// raw colour — regresses here instead of in staging. Each needle
	// gates a distinct concern: page container, card-wrapped section,
	// table chrome, tabular numerals on metrics, and token usage.
	for _, needle := range []string{
		".dashboard",
		".dashboard__section",
		".dashboard__table",
		"tabular-nums",
		"var(--border-subtle",
		"var(--text-strong",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("dashboard.css missing required selector or token reference %q", needle)
		}
	}
}
