package main

// SIN-63294 — regression guard for the baseline auth stylesheet.
//
// internal/adapter/httpapi/views/layout.html references
// /static/css/auth.css; if the file is missing on disk the link tag
// 404s silently and the /login and /hello-tenant pages render with
// user-agent defaults — the exact "tela sem formatação" bug that
// motivated this ticket. Spinning up the same FileServer setup that
// customdomain_wire.go mounts in production proves the asset exists
// and is served as text/css through the registered static handler.
//
// This is parallel to the SIN-62916 TestPrivacyStylesheet_ServedAsCSS
// in privacy_wire_test.go; both run in the cmd/server package because
// that is where the static-route wiring lives.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthStylesheet_ServedAsCSS(t *testing.T) {
	t.Parallel()
	// cmd/server lives two levels below the repo root, so the
	// web/static tree is at ../../web/static when go test runs from
	// the package directory.
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/auth.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/auth.css must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("served body is empty — auth.css must have rules")
	}
	// Spot-check the load-bearing selectors so a future refactor of
	// auth.css that drops baseline form/button styling regresses the
	// SIN-63294 bug here instead of in staging. Each selector below
	// gates a distinct visual concern called out by the ticket:
	// layout container, form label, text input, submit button,
	// alert role.
	for _, needle := range []string{
		"main",
		"label",
		`input[type="email"]`,
		`button[type="submit"]`,
		`p[role="alert"]`,
		// Tenant theme integration — without --color-primary the
		// button stays GitHub-primer blue regardless of branding.
		// Pin the var() reference so a future refactor that hard-codes
		// the colour back to #1f6feb is caught by this test.
		"var(--color-primary",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("auth.css missing required selector or token reference %q", needle)
		}
	}
}
