package main

// SIN-63977 / SEC-F1 — regression guard for the master grants
// kind-toggle script. internal/web/master/grants_templates.go's
// grantsLayoutTmpl references /static/js/master-grants.js; if the
// file is missing on disk the script tag 404s under strict CSP and
// the kind-radio toggle becomes a dead button without breaking a
// single Go test. Mirrors TestAppShellToggleScript_ServedAsJS in
// design_system_js_static_test.go.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMasterGrantsToggleScript_ServedAsJS(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/js/master-grants.js", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — /static/js/master-grants.js missing on disk", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "javascript")
	}

	body := rec.Body.String()
	if len(body) < 200 {
		t.Fatalf("body too short (%d bytes) — master-grants.js stub or empty?", len(body))
	}

	// Pin one needle per behaviour the script must wire so a future
	// refactor that drops the data-attribute selector or the toggle
	// loop fails this guard rather than silently breaking the
	// kind-radio UX.
	for _, needle := range []string{
		`data-grant-fields`,
		`data-grant-kind-toggle`,
		`name="kind"`,
		`addEventListener`,
		`hidden`,
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("master-grants.js missing %q — kind-toggle wiring incomplete", needle)
		}
	}
}
