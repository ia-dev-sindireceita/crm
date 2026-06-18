package main

// SIN-65125 — regression guard for the hello-tenant content stylesheet.
//
// /hello-tenant renders inside the app-shell but also pulls the legacy
// auth.css via head_extra. auth.css hard-binds body/main to the
// non-flipping legacy tenant tokens, so in dark mode the card copy and
// "Abrir" links rendered low-contrast on the dark app surface (30 axe
// color-contrast failures). hello-tenant.css loads after auth.css and
// rebinds the content to the theme-aware Peitho tokens so the whole
// content area flips with data-theme="dark".
//
// If the file 404s the dark-mode contrast bug returns silently. Pin its
// presence + the load-bearing theme-aware token references here, the
// same way auth_css_static_test.go / dashboard_css_static_test.go pin
// their sheets.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHelloTenantStylesheet_ServedAsCSS(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/hello-tenant.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/hello-tenant.css must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("served body is empty — hello-tenant.css must have rules")
	}

	// The card + link rules must consume the theme-aware tokens that
	// flip under data-theme="dark". A refactor that hard-codes a hex
	// or rebinds to a non-flipping legacy --color-* token reintroduces
	// the SIN-65125 dark-mode contrast failures; pin the load-bearing
	// selectors and token references so that regresses here.
	for _, needle := range []string{
		".hello-tenant__card",
		".hello-tenant__card-link",
		".hello-tenant__card-disabled",
		"var(--surface-0)",
		"var(--text-strong)",
		"var(--text-muted)",
		"var(--color-link)",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("hello-tenant.css missing %q — dark-mode contrast guard", needle)
		}
	}

	// Guard against any raw hex literal on a color/background property:
	// every visible color in this sheet must come from a token so it
	// flips with the theme. (Comments may mention hex — strip them
	// first.) outline/box-shadow alpha values live in tokens, not here.
	for _, banned := range []string{
		"color: #",
		"background: #",
		"background-color: #",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("hello-tenant.css contains hard-coded %q — use a theme-aware token instead", banned)
		}
	}
}
