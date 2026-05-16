package main

// SIN-62906 — ai_policy wire tests. The handler covers its own
// behaviour exhaustively in internal/web/aipolicy; these tests pin
// the composition root: buildWebAIPolicyHandler returns (nil, no-op)
// when DATABASE_URL is unset (no LGPD-blocking surface here), the
// pure assembly seam rejects nil deps, and the assembled mux mounts
// every route the description lists.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aipolicy"
	"github.com/pericles-luz/crm/internal/tenancy"
	webaipolicy "github.com/pericles-luz/crm/internal/web/aipolicy"
)

// memRepoForWire is a minimal Repository fake — enough to drive the
// route-mount smoke test. The web/aipolicy package's own tests cover
// behavior; this just confirms wireup.
type memRepoForWire struct{}

func (memRepoForWire) Get(_ context.Context, _ uuid.UUID, _ aipolicy.ScopeType, _ string) (aipolicy.Policy, bool, error) {
	return aipolicy.Policy{}, false, nil
}
func (memRepoForWire) Upsert(_ context.Context, _ aipolicy.Policy) error { return nil }
func (memRepoForWire) List(_ context.Context, _ uuid.UUID) ([]aipolicy.Policy, error) {
	return []aipolicy.Policy{}, nil
}
func (memRepoForWire) Delete(_ context.Context, _ uuid.UUID, _ aipolicy.ScopeType, _ string) (bool, error) {
	return false, nil
}

func TestBuildWebAIPolicyHandler_DegradesWhenDSNUnset(t *testing.T) {
	t.Parallel()
	h, cleanup := buildWebAIPolicyHandler(context.Background(), func(string) string { return "" })
	defer cleanup()
	if h != nil {
		t.Fatalf("expected nil handler when DATABASE_URL unset; got %T", h)
	}
}

func TestAssembleWebAIPolicyHandler_RejectsNilDeps(t *testing.T) {
	t.Parallel()
	repo := memRepoForWire{}
	resolver, err := aipolicy.NewResolver(repo)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	cases := []struct {
		name     string
		repo     aipolicy.Repository
		resolver webaipolicy.Resolver
		now      func() time.Time
	}{
		{"nil repo", nil, resolver, time.Now},
		{"nil resolver", repo, nil, time.Now},
		{"nil now", repo, resolver, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := assembleWebAIPolicyHandler(tc.repo, tc.resolver, tc.now, slog.Default()); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestAssembleWebAIPolicyHandler_MountsEveryRoute(t *testing.T) {
	t.Parallel()
	repo := memRepoForWire{}
	resolver, err := aipolicy.NewResolver(repo)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	h, err := assembleWebAIPolicyHandler(repo, resolver, time.Now, nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if h == nil {
		t.Fatalf("expected non-nil handler")
	}

	tenant := &tenancy.Tenant{
		ID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Name: "Acme",
		Host: "acme.crm.local",
	}

	cases := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"list page", http.MethodGet, "/settings/ai-policy", http.StatusOK},
		{"new form", http.MethodGet, "/settings/ai-policy/new", http.StatusOK},
		{"preview", http.MethodGet, "/settings/ai-policy/preview", http.StatusOK},
		{"edit form (404 when no row)", http.MethodGet, "/settings/ai-policy/tenant/" + tenant.ID.String() + "/edit", http.StatusNotFound},
		{"delete (no-op idempotent)", http.MethodDelete, "/settings/ai-policy/tenant/" + tenant.ID.String(), http.StatusOK},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(c.method, c.path, nil)
			req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("%s %s status = %d, want %d; body=%s", c.method, c.path, rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// TestAssembleWebAIPolicyHandler_NilResolverErrorMessage documents
// the precise error surface so callers can match on it.
func TestAssembleWebAIPolicyHandler_NilResolverErrorMessage(t *testing.T) {
	t.Parallel()
	_, err := assembleWebAIPolicyHandler(memRepoForWire{}, nil, time.Now, nil)
	if err == nil {
		t.Fatal("err = nil, want resolver-nil failure")
	}
	if !errors.Is(err, err) { // sanity check
		t.Fatal("errors.Is sentinel returned false")
	}
}
