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
//
// SIN-62918 adds the aipolicy-adapter coverage: the wire now wraps
// the SIN-62351 cascade resolver into webprivacy.ModelResolver, and
// buildPrivacyModelResolver falls back to staticModelResolver when
// DATABASE_URL is unset.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aipolicy"
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

// stubPolicyResolver is the in-process aipolicy.Resolver substitute
// used by the SIN-62918 wire tests. It records every Resolve call so
// the adapter assertion can confirm the wire only ever asks for the
// tenant scope (no channel / no team — the privacy page is tenant-
// level by design).
type stubPolicyResolver struct {
	model string
	src   aipolicy.ResolveSource
	err   error
	calls []aipolicy.ResolveInput
}

func (s *stubPolicyResolver) Resolve(_ context.Context, in aipolicy.ResolveInput) (aipolicy.Policy, aipolicy.ResolveSource, error) {
	s.calls = append(s.calls, in)
	if s.err != nil {
		return aipolicy.Policy{}, "", s.err
	}
	return aipolicy.Policy{TenantID: in.TenantID, Model: s.model}, s.src, nil
}

// TestAipolicyModelResolver_ReturnsConfiguredModel covers AC #1: a
// tenant with an ai_policy row visible to the cascade sees its
// configured model on /settings/privacy.
func TestAipolicyModelResolver_ReturnsConfiguredModel(t *testing.T) {
	t.Parallel()
	stub := &stubPolicyResolver{model: "openrouter/anthropic/claude-3.5-sonnet", src: aipolicy.SourceTenant}
	adapter := aipolicyModelResolver{resolver: stub}
	tenantID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	got, err := adapter.ActiveModel(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("ActiveModel: %v", err)
	}
	if got != "openrouter/anthropic/claude-3.5-sonnet" {
		t.Errorf("model = %q, want %q", got, "openrouter/anthropic/claude-3.5-sonnet")
	}
	if len(stub.calls) != 1 {
		t.Fatalf("Resolve calls = %d, want 1", len(stub.calls))
	}
	if stub.calls[0].TenantID != tenantID {
		t.Errorf("Resolve tenant = %s, want %s", stub.calls[0].TenantID, tenantID)
	}
	if stub.calls[0].ChannelID != nil || stub.calls[0].TeamID != nil {
		t.Errorf("expected tenant-scope-only input, got channel=%v team=%v",
			stub.calls[0].ChannelID, stub.calls[0].TeamID)
	}
}

// TestAipolicyModelResolver_ReturnsDefaultModelForUnconfiguredTenant
// covers AC #2: a tenant without an ai_policy row sees
// "openrouter/auto" — the cascade's SourceDefault returns DefaultPolicy
// which carries that model string verbatim.
func TestAipolicyModelResolver_ReturnsDefaultModelForUnconfiguredTenant(t *testing.T) {
	t.Parallel()
	stub := &stubPolicyResolver{model: aipolicy.DefaultPolicy().Model, src: aipolicy.SourceDefault}
	adapter := aipolicyModelResolver{resolver: stub}

	got, err := adapter.ActiveModel(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("ActiveModel: %v", err)
	}
	if got != webprivacy.FallbackModel {
		t.Errorf("model = %q, want %q (FallbackModel must match DefaultPolicy)", got, webprivacy.FallbackModel)
	}
}

// TestAipolicyModelResolver_BubblesResolverError covers AC #3 from
// the adapter side: when the resolver fails, the adapter returns
// the error so the privacy handler's existing fail-soft path renders
// FallbackModel and logs the failure (TestView_FallsBackOnResolverError
// in internal/web/privacy proves the handler half of the contract).
func TestAipolicyModelResolver_BubblesResolverError(t *testing.T) {
	t.Parallel()
	want := errors.New("aipolicy/postgres: Get: connection refused")
	stub := &stubPolicyResolver{err: want}
	adapter := aipolicyModelResolver{resolver: stub}

	got, err := adapter.ActiveModel(context.Background(), uuid.New())
	if err == nil {
		t.Fatalf("expected error, got nil (model = %q)", got)
	}
	if !errors.Is(err, want) {
		t.Errorf("error not wrapped: got %v, want chain containing %v", err, want)
	}
	if got != "" {
		t.Errorf("expected empty model on error, got %q", got)
	}
}

// TestBuildPrivacyModelResolver_FallsBackWhenDSNUnset proves the
// cmd/server-tests / smoke-runs path: with no DATABASE_URL set the
// wire still produces a working ModelResolver that returns
// FallbackModel without owning any pool. The cleanup must be a noop
// in that path.
func TestBuildPrivacyModelResolver_FallsBackWhenDSNUnset(t *testing.T) {
	t.Parallel()
	r, cleanup := buildPrivacyModelResolver(context.Background(), func(string) string { return "" })
	defer cleanup()

	if r == nil {
		t.Fatal("buildPrivacyModelResolver must always return a non-nil ModelResolver")
	}
	if _, ok := r.(staticModelResolver); !ok {
		t.Fatalf("expected staticModelResolver fallback when DSN unset, got %T", r)
	}
	got, err := r.ActiveModel(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("ActiveModel: %v", err)
	}
	if got != webprivacy.FallbackModel {
		t.Errorf("fallback model = %q, want %q", got, webprivacy.FallbackModel)
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
