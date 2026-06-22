package main

// SIN-65099 / Pitho C4 — regression guards for the Contacts + LGPD
// stylesheets.
//
//   1. The LGPD admin templates (internal/web/lgpd/ui.go) link
//      /static/css/lgpd.css. That file did not exist before this ticket,
//      so the link 404'd and the pages rendered with user-agent defaults
//      (the same "tela sem formatação" class of bug auth_css_static_test
//      guards for auth.css). This proves the asset now exists on disk
//      and is served as text/css.
//
//   2. Both contacts.css and lgpd.css were ported to the design-system
//      tokens. The Pitho bar forbids raw hex colour literals — every
//      colour must consume a tokens.css custom property so per-tenant
//      branding + dark mode work. These guards fail if a future edit
//      reintroduces a bare #rrggbb / #rgb literal or drops token usage.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// rawHexColor matches a bare 3/4/6/8-digit hex colour literal. CSS id
// selectors (#foo) never start with three hex characters in these
// sheets, so this does not false-positive on selectors.
var rawHexColor = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)

func TestLGPDStylesheet_ServedAsCSS(t *testing.T) {
	t.Parallel()
	// cmd/server lives two levels below the repo root, so the web/static
	// tree is at ../../web/static when go test runs from the package dir.
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/lgpd.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/lgpd.css must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("served body is empty — lgpd.css must have rules")
	}
	// Spot-check class names actually used by internal/web/lgpd/ui.go so a
	// future template rename does not silently desync from the sheet. Each
	// gates a distinct visual concern (shell, panel, primary/danger
	// buttons, status badge, empty state).
	for _, needle := range []string{
		".lgpd-shell",
		".lgpd-panel",
		".lgpd-btn--primary",
		".lgpd-btn--danger",
		".lgpd-badge--in_retention",
		".lgpd-badge--completed",
		".lgpd-empty",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("lgpd.css missing required selector %q", needle)
		}
	}
}

func TestPithoC4_StylesheetsAreTokenOnly(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"contacts.css", "lgpd.css"} {
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
