package main

// SIN-62919 — contacts stylesheet wire test. The contacts identity
// page template (internal/web/contacts/templates.go) links to
// /static/css/contacts.css; a missing file there silently 404s
// without surfacing in any handler-level test. Spinning up the
// same FileServer setup that customdomain_wire.go mounts in
// production proves the asset exists on disk and is served as
// text/css through the registered static handler.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContactsStylesheet_ServedAsCSS(t *testing.T) {
	t.Parallel()
	// cmd/server lives two levels below the repo root, so the
	// web/static tree is at ../../web/static when go test runs
	// from the package directory.
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/contacts.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/contacts.css must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("served body is empty — contacts.css must have rules")
	}
	// Spot-check class names actually used by
	// internal/web/contacts/templates.go so a future template
	// rename does not silently desync from the stylesheet. The
	// four selectors below each gate a distinct visual concern
	// (shell, panel container, link row, destructive split
	// button).
	for _, needle := range []string{
		".contact-shell",
		".identity-panel",
		".identity-link",
		".identity-link__split",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("contacts.css missing required selector %q", needle)
		}
	}
}
