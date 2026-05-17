package main

// SIN-62919 — inbox stylesheet wire test. The inbox layout template
// (internal/web/inbox/templates.go) links to /static/css/inbox.css;
// a missing file there silently 404s without surfacing in any
// handler-level test. Spinning up the same FileServer setup that
// customdomain_wire.go mounts in production proves the asset
// exists on disk and is served as text/css through the registered
// static handler.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInboxStylesheet_ServedAsCSS(t *testing.T) {
	t.Parallel()
	// cmd/server lives two levels below the repo root, so the
	// web/static tree is at ../../web/static when go test runs
	// from the package directory.
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/inbox.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/inbox.css must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("served body is empty — inbox.css must have rules")
	}
	// Spot-check class names actually used by
	// internal/web/inbox/templates.go so a future template
	// rename does not silently desync from the stylesheet. The
	// four selectors below each gate a distinct visual concern
	// (two-pane shell, list link, outbound bubble, status badge).
	for _, needle := range []string{
		".inbox-shell",
		".conversation-list__link",
		".message-bubble",
		".message-bubble__status",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("inbox.css missing required selector %q", needle)
		}
	}
}
