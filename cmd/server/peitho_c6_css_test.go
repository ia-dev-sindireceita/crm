package main

// SIN-65101 / Peitho C6 — regression guards for the Settings cluster
// stylesheets (privacy/DPA + AI-policy audit log).
//
//   1. The aipolicyaudit template (internal/web/aipolicyaudit/templates.go)
//      links /static/css/aipolicyaudit.css. That file did not exist before
//      this ticket, so the link 404'd and /settings/ai-policy/audit
//      rendered with user-agent defaults (the "tela sem formatação" class
//      of bug the C4/C5 guards cover). This proves the asset now exists on
//      disk and is served as text/css.
//
//   2. privacy.css carried raw GitHub-primer hex literals; it is now
//      ported to the design-system tokens so per-tenant branding + dark
//      mode work. The Peitho bar forbids raw hex on screen — every colour
//      must consume a tokens.css custom property.
//
//      EXCEPTION: privacy.css keeps an `@media print` block whose colours
//      are intentionally forced black/white (board-ratified archival
//      print, SIN-62917). Those raw values must NOT become themeable, so
//      the token-only guard scopes itself to the on-screen rules (the
//      portion before `@media print`).
//
// rawHexColor is defined in peitho_c4_css_test.go (same package).

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestPeithoC6_AuditStylesheet_ServedAsCSS(t *testing.T) {
	t.Parallel()
	// cmd/server lives two levels below the repo root, so the web/static
	// tree is at ../../web/static when go test runs from the package dir.
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/aipolicyaudit.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/aipolicyaudit.css must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("served body is empty — aipolicyaudit.css must have rules")
	}
	// Spot-check the load-bearing selectors emitted by
	// internal/web/aipolicyaudit/templates.go so a future template rename
	// does not silently desync from the sheet. Each gates a distinct
	// visual concern (shell, filter bar, table, master-impersonation tint).
	for _, needle := range []string{
		".audit-shell",
		".audit-filters__form",
		".audit-table",
		".audit-row--master",
		".audit-pill--master",
		"var(--color-primary",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("aipolicyaudit.css missing required selector or token reference %q", needle)
		}
	}
}

func TestPeithoC6_SettingsStylesheetsAreTokenOnly(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"privacy.css", "aipolicyaudit.css"} {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile("../../web/static/css/" + name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			body := string(raw)
			// Scope the hex check to the on-screen rules: the @media print
			// block in privacy.css is deliberately forced B/W (SIN-62917)
			// and is exempt from the token-only bar.
			screen := body
			if i := strings.Index(body, "@media print"); i >= 0 {
				screen = body[:i]
			}
			if hits := rawHexColor.FindAllString(screen, -1); len(hits) > 0 {
				t.Errorf("%s has raw hex colour literals %v in its on-screen rules — use tokens.css custom properties instead", name, hits)
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
