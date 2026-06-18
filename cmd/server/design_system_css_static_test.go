package main

// SIN-63935 — regression guard for the design-system foundation
// stylesheets. internal/web/shell/layout.html references
// /static/css/tokens.css, /static/css/components.css, and
// /static/css/app-shell.css; if any is missing on disk the link tag
// 404s silently and every feature mounted on shell.Layout renders
// without tokens. Parallel to TestAuthStylesheet_ServedAsCSS in
// auth_css_static_test.go.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDesignSystemStylesheets_ServedAsCSS(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	cases := []struct {
		path     string
		needles  []string
		minSize  int
		descript string
	}{
		{
			path: "/static/css/tokens.css",
			// Pin one needle per token band declared in the issue
			// spec so a future refactor that drops a band fails here
			// rather than in staging.
			needles: []string{
				"--surface-0",
				"--text-default",
				"--color-primary",
				"--color-success",
				"--color-warning",
				"--color-danger",
				"--space-4",
				"--text-base",
				"--font-sans",
				"--radius-md",
				"--shadow-1",
				"--motion-base",
				"--ease-out",
				"--bp-sm",
				"--hit-target-min",
				"prefers-reduced-motion",
			},
			minSize:  500,
			descript: "tokens.css must contain every declared band",
		},
		{
			path: "/static/css/components.css",
			needles: []string{
				".btn",
				".btn--primary",
				".btn--ghost",
				".btn--danger",
				".badge",
				".card",
				".empty-state",
				".alert--info",
				".alert--danger",
				".table",
				".modal",
				".field",
				"var(--color-primary)",
			},
			minSize:  500,
			descript: "components.css must contain every primitive class",
		},
		{
			path: "/static/css/app-shell.css",
			needles: []string{
				".app-shell__sidebar",
				".app-shell__brand",
				".app-shell__nav",
				".app-shell__user-menu",
				".app-shell__hamburger",
				".app-shell__main",
				"var(--hit-target-min)",
				`aria-current="page"`,
				"@media (max-width: 899px)",
			},
			minSize:  500,
			descript: "app-shell.css must contain sidebar + nav + user-menu + hamburger + mobile breakpoint",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — %s missing on disk", rec.Code, tc.path)
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
				t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
			}
			body := rec.Body.String()
			if len(body) < tc.minSize {
				t.Fatalf("body too short (%d bytes) — %s", len(body), tc.descript)
			}
			for _, needle := range tc.needles {
				if !strings.Contains(body, needle) {
					t.Errorf("body missing %q — %s", needle, tc.descript)
				}
			}
		})
	}
}
