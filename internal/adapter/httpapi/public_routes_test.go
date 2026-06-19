package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
)

// TestPublicRoutes_Allowlist asserts the canonical public set. The
// list is small on purpose — every entry here is an audit-bearing
// decision that a route MAY be reached without RequireAuth.
func TestPublicRoutes_Allowlist(t *testing.T) {
	t.Parallel()
	want := []struct {
		method, pattern string
	}{
		{http.MethodGet, "/health"},
		{http.MethodGet, "/metrics"},
		{http.MethodPost, "/internal/test-alert"},
		{http.MethodGet, "/login"},
		{http.MethodPost, "/login"},
		{http.MethodGet, "/m/login"},
		{http.MethodPost, "/m/login"},
		// SIN-65232: /m/logout is POST-only (forced-logout CSRF fix).
		{http.MethodPost, "/m/logout"},
	}
	got := httpapi.PublicRoutes()
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d want=%d", len(got), len(want))
	}
	gotSet := map[string]bool{}
	for _, r := range got {
		gotSet[r.Method+" "+r.Pattern] = true
		if r.Reason == "" {
			t.Errorf("route %q %q missing Reason", r.Method, r.Pattern)
		}
	}
	for _, w := range want {
		if !gotSet[w.method+" "+w.pattern] {
			t.Errorf("expected public route %q %q missing", w.method, w.pattern)
		}
	}
}

func TestPublicRoutes_IsPublic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		method, pattern string
		want            bool
	}{
		{http.MethodGet, "/health", true},
		{http.MethodGet, "/login", true},
		{http.MethodPost, "/login", true},
		{http.MethodGet, "/hello-tenant", false},
		{http.MethodPost, "/logout", false},
		{http.MethodGet, "/m/2fa/enroll", false},
	}
	for _, c := range cases {
		got := httpapi.IsPublic(c.method, c.pattern)
		if got != c.want {
			t.Errorf("IsPublic(%q, %q) = %v want %v", c.method, c.pattern, got, c.want)
		}
	}
}

// TestPublicRoutes_CopyIsolation asserts callers cannot mutate the
// shared list via the returned slice.
func TestPublicRoutes_CopyIsolation(t *testing.T) {
	t.Parallel()
	got := httpapi.PublicRoutes()
	if len(got) == 0 {
		t.Skip("no public routes registered")
	}
	got[0].Pattern = "/POISONED"
	if httpapi.IsPublic(got[0].Method, "/POISONED") {
		t.Fatal("public list leaked: returned slice mutates the source")
	}
}
