package main

// SIN-62354 — privacy wire tests. The handler covers its own behaviour
// exhaustively in internal/web/privacy; these tests pin the composition
// root: buildWebPrivacyHandler always returns a non-nil handler (LGPD
// disclosure cannot fail-soft), assembleWebPrivacyHandler rejects nil
// deps, the assembled mux mounts both routes, and the static model
// resolver returns the documented fallback.
//
// SIN-62916 adds the static-asset coverage: the privacy template
// references /static/css/privacy.css, and a missing file there
// silently 404s without surfacing in any handler-level test.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/tenancy"
	webprivacy "github.com/pericles-luz/crm/internal/web/privacy"
)

func TestBuildWebPrivacyHandler_NonNilByContract(t *testing.T) {
	t.Parallel()
	h, cleanup := buildWebPrivacyHandler(context.Background(), func(string) string { return "" })
	defer cleanup()
	if h == nil {
		t.Fatalf("buildWebPrivacyHandler must always return a non-nil handler — LGPD disclosure is release-blocking")
	}
}

func TestAssembleWebPrivacyHandler_RejectsNilDeps(t *testing.T) {
	t.Parallel()
	resolver := staticModelResolver{model: webprivacy.FallbackModel}
	cases := []struct {
		name     string
		resolver webprivacy.ModelResolver
		now      webprivacy.Now
	}{
		{"nil resolver", nil, time.Now},
		{"nil now", resolver, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := assembleWebPrivacyHandler(tc.resolver, tc.now, nil); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestAssembleWebPrivacyHandler_MountsBothRoutes(t *testing.T) {
	t.Parallel()
	resolver := staticModelResolver{model: webprivacy.FallbackModel}
	h, err := assembleWebPrivacyHandler(resolver, time.Now, nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if h == nil {
		t.Fatalf("expected non-nil handler")
	}

	tenant := &tenancy.Tenant{
		ID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Name: "Acme Cobranças",
		Host: "acme.crm.local",
	}

	cases := []struct {
		name   string
		path   string
		want   int
		needle string
	}{
		{"page renders", "/settings/privacy", http.StatusOK, "OpenRouter"},
		{"DPA download", "/settings/privacy/dpa.md", http.StatusOK, "OpenRouter"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, c.path, nil)
			req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
			if !strings.Contains(rec.Body.String(), c.needle) {
				t.Errorf("body missing required substring %q", c.needle)
			}
		})
	}
}

func TestStaticModelResolver_ReturnsConfiguredModel(t *testing.T) {
	t.Parallel()
	r := staticModelResolver{model: "openrouter/anthropic/haiku"}
	got, err := r.ActiveModel(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("ActiveModel returned error: %v", err)
	}
	if got != "openrouter/anthropic/haiku" {
		t.Errorf("ActiveModel = %q, want %q", got, "openrouter/anthropic/haiku")
	}
}

// TestPrivacyStylesheet_ServedAsCSS is the SIN-62916 regression
// guard: the privacy page template references
// /static/css/privacy.css; if the file is missing, the link tag
// 404s silently and the page renders unstyled. Spinning up the
// same FileServer setup that customdomain_wire.go mounts in
// production proves the asset exists on disk and is served as
// text/css through the registered static handler. AC #1.
func TestPrivacyStylesheet_ServedAsCSS(t *testing.T) {
	t.Parallel()
	// cmd/server lives two levels below the repo root, so the
	// web/static tree is at ../../web/static when go test runs
	// from the package directory.
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/privacy.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/privacy.css must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("served body is empty — privacy.css must have rules")
	}
	// Spot-check a class actually used by the template so a
	// future template rename does not silently desync from the
	// stylesheet. The four most load-bearing class names below
	// each gate a distinct visual concern (shell, lede, model
	// callout, pending row tint).
	for _, needle := range []string{
		".privacy-shell",
		".privacy-lede",
		".privacy-model__value",
		".privacy-row--pending",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("privacy.css missing required selector %q", needle)
		}
	}
}
