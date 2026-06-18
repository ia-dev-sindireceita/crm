package main

// SIN-65100 / Peitho C5 — regression guards for the Campaigns + Catalog
// stylesheets.
//
//   1. The catalog templates (internal/web/catalog/templates.go) shipped
//      WITHOUT any linked stylesheet, so /catalog and /catalog/{id}
//      rendered with user-agent defaults — the "tela sem formatação" bug
//      auth_css_static_test / peitho_c4 (lgpd.css) guard for their
//      screens. This proves catalog.css now exists on disk and is served
//      as text/css through the same FileServer customdomain_wire.go mounts.
//
//   2. campaigns.css had raw GitHub-primer hex literals and only resolved
//      its tokens via var() fallbacks (tokens.css was never linked on the
//      campaigns pages, so the fallbacks were what actually rendered).
//      Both sheets are now token-only; the Peitho bar forbids raw hex so
//      per-tenant branding + dark mode work. These guards fail if a future
//      edit reintroduces a bare #rrggbb / #rgb literal or drops tokens.
//
// rawHexColor is defined in peitho_c4_css_test.go (same package).

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestCatalogStylesheet_ServedAsCSS(t *testing.T) {
	t.Parallel()
	// cmd/server lives two levels below the repo root, so the web/static
	// tree is at ../../web/static when go test runs from the package dir.
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/catalog.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/catalog.css must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("served body is empty — catalog.css must have rules")
	}
	// Spot-check the load-bearing selectors actually emitted by
	// internal/web/catalog/templates.go so a future template rename does
	// not silently desync from the sheet. Each gates a distinct visual
	// concern (shell, sidebar, product table, argument editor, preview).
	for _, needle := range []string{
		".catalog-shell",
		".catalog-sidebar",
		".catalog-list",
		".catalog-arg-editor__columns",
		".catalog-prompt-preview",
		"var(--color-primary",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("catalog.css missing required selector or token reference %q", needle)
		}
	}
}

func TestPeithoC5_StylesheetsAreTokenOnly(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"campaigns.css", "catalog.css"} {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile("../../web/static/css/" + name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			body := string(raw)
			if hits := rawHexColor.FindAllString(body, -1); len(hits) > 0 {
				t.Errorf("%s has raw hex colour literals %v — use tokens.css custom properties instead", name, hits)
			}
			// A token-ported sheet must reference the custom properties.
			if !strings.Contains(body, "var(--color-") {
				t.Errorf("%s does not reference any --color-* token", name)
			}
			if !strings.Contains(body, "var(--space-") {
				t.Errorf("%s does not reference any --space-* token", name)
			}
		})
	}
}
