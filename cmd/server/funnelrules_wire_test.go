package main

// SIN-62961 — wire-level smoke tests for the funnel-rules editor.
//
// The handler-package tests cover the HTMX shape; these tests prove
// the wire and the static stylesheet the page links to exist and
// serve cleanly. A missing stylesheet would 404 silently in production
// because the template embeds the link at HTML render time.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	domain "github.com/pericles-luz/crm/internal/funnel/rules"
)

func TestAssembleWebFunnelRulesHandler_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	if _, err := assembleWebFunnelRulesHandler(nil, time.Now, nil); err == nil {
		t.Fatal("expected error when store is nil")
	}
	if _, err := assembleWebFunnelRulesHandler(domain.NewInMemoryRepository(), nil, nil); err == nil {
		t.Fatal("expected error when now is nil")
	}
}

func TestAssembleWebFunnelRulesHandler_BuildsMuxAndServesRoutes(t *testing.T) {
	t.Parallel()
	h, err := assembleWebFunnelRulesHandler(domain.NewInMemoryRepository(), time.Now, nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	// The handler reads tenant + CSRF from the request context. At the
	// wire layer those return empty when there is no session; we only
	// need to confirm the mux routes the request rather than 404 — that
	// proves Routes() registered every endpoint.
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/funnel/rules"},
		{http.MethodGet, "/funnel/rules/new"},
		{http.MethodPost, "/funnel/rules"},
		{http.MethodGet, "/funnel/rules/trigger-fields"},
		{http.MethodGet, "/funnel/rules/action-fields"},
		{http.MethodGet, "/funnel/rules/preview"},
		{http.MethodGet, "/funnel/rules/00000000-0000-0000-0000-000000000000/edit"},
		{http.MethodPatch, "/funnel/rules/00000000-0000-0000-0000-000000000000"},
		{http.MethodPatch, "/funnel/rules/00000000-0000-0000-0000-000000000000/toggle"},
		{http.MethodDelete, "/funnel/rules/00000000-0000-0000-0000-000000000000"},
	} {
		req := httptest.NewRequest(route.method, route.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound && rec.Body.String() == "404 page not found\n" {
			t.Errorf("route %s %s returned 404 — Routes() did not register it", route.method, route.path)
		}
	}
}

func TestAssembleWebFunnelRulesHandler_NilLoggerDefaults(t *testing.T) {
	t.Parallel()
	if _, err := assembleWebFunnelRulesHandler(domain.NewInMemoryRepository(), time.Now, nil); err != nil {
		t.Fatalf("expected nil error when logger is nil, got %v", err)
	}
}

func TestFunnelRulesStylesheet_ServedAsCSS(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/funnel-rules.css", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/funnel-rules.css must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("served body is empty — funnel-rules.css must have rules")
	}
	for _, needle := range []string{
		".funnel-rules-shell",
		".funnel-rules-table",
		".funnel-rules-row",
		".funnel-rules-form",
		".funnel-rules-preview",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("funnel-rules.css missing required selector %q", needle)
		}
	}
}
