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

// SIN-65575 — the public stdlib mux delegates a prefix to the chi router
// only if that prefix is present in iamRoutes. SIN-64977 added the chi
// handler for the exact GET /contacts page (router.go contactsRead) but
// iamRoutes only carried the "/contacts/" subtree. Without the exact
// "/contacts" pattern, the stdlib mux applied its subtree→canonical
// redirect (/contacts → /contacts/), and since chi has no index handler
// for /contacts/, the request 404'd. Every analogous surface
// (funnel/catalog/campaigns/inbox) carries both variants; contacts was
// the only one missing the exact pattern. This assertion catches a
// regression that drops either the exact "/contacts" page route or the
// "/contacts/" subtree.
func TestIAMRoutesIncludesContacts(t *testing.T) {
	t.Parallel()
	want := map[string]bool{"/contacts": false, "/contacts/": false}
	for _, r := range iamRoutes {
		if _, ok := want[r]; ok {
			want[r] = true
		}
	}
	for route, found := range want {
		if !found {
			t.Errorf("iamRoutes does not contain %q — the SIN-64977 contacts mount would be unreachable (GET /contacts 404s at the subtree redirect)", route)
		}
	}
}
