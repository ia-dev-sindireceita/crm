package main

// SIN-65112 / Pitho C6b — regression guard for the branding (identidade
// visual) stylesheet.
//
//   1. internal/web/branding/templates.go ("branding.page") now links
//      /static/css/branding.css. That file did not exist before this
//      ticket — the page <head> linked NO stylesheet at all, so the shell,
//      upload form, colour preview card, swatches and action buttons
//      rendered with user-agent defaults (the "tela sem formatação" class
//      of bug the C4–C6 guards cover). This proves the asset now exists on
//      disk and is served as text/css.
//
//   2. branding.css must be token-only: the Pitho bar forbids raw hex on
//      screen — every colour must consume a tokens.css custom property so
//      per-tenant branding overrides and dark mode work.
//
// rawHexColor is defined in pitho_c4_css_test.go (same package).

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestPithoC6b_BrandingStylesheet_ServedAsCSS(t *testing.T) {
	t.Parallel()
	// cmd/server lives two levels below the repo root, so the web/static
	// tree is at ../../web/static when go test runs from the package dir.
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/branding.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/branding.css must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("served body is empty — branding.css must have rules")
	}
	// Spot-check the load-bearing selectors emitted by
	// internal/web/branding/templates.go so a future template rename does
	// not silently desync from the sheet. Each gates a distinct visual
	// concern (shell, upload form, preview plate, swatches, actions, flash).
	for _, needle := range []string{
		".branding-shell",
		".branding-upload-form",
		".branding-preview-plate",
		".branding-swatch-input",
		".branding-action--save",
		".branding-flash--error",
		"var(--color-primary",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("branding.css missing required selector or token reference %q", needle)
		}
	}
}

func TestPithoC6b_BrandingStylesheetIsTokenOnly(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../web/static/css/branding.css")
	if err != nil {
		t.Fatalf("read branding.css: %v", err)
	}
	body := string(raw)
	if hits := rawHexColor.FindAllString(body, -1); len(hits) > 0 {
		t.Errorf("branding.css has raw hex colour literals %v — use tokens.css custom properties instead", hits)
	}
	for _, token := range []string{"var(--color-", "var(--space-", "var(--radius-"} {
		if !strings.Contains(body, token) {
			t.Errorf("branding.css does not reference any %s token", token)
		}
	}
}
