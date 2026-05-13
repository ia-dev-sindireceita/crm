package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/tenancy"
)

type stubResolver struct {
	tenant  *tenancy.Tenant
	err     error
	gotHost string
}

func (s *stubResolver) ResolveByHost(_ context.Context, host string) (*tenancy.Tenant, error) {
	s.gotHost = host
	return s.tenant, s.err
}

// captureHandler records the tenant the middleware injected so we can
// assert downstream visibility.
func captureHandler(out **tenancy.Tenant) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, err := tenancy.FromContext(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		*out = t
		w.WriteHeader(http.StatusOK)
	})
}

func TestTenantScope_AttachesTenantOnSuccess(t *testing.T) {
	t.Parallel()

	want := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	resolver := &stubResolver{tenant: want}

	var got *tenancy.Tenant
	h := middleware.TenantScope(resolver)(captureHandler(&got))

	req := httptest.NewRequest(http.MethodGet, "https://acme.crm.local/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if got != want {
		t.Fatalf("downstream got %#v, want %#v", got, want)
	}
	if resolver.gotHost != "acme.crm.local" {
		t.Fatalf("resolver host = %q, want acme.crm.local", resolver.gotHost)
	}
}

func TestTenantScope_NotFoundIsGeneric404(t *testing.T) {
	t.Parallel()

	resolver := &stubResolver{err: tenancy.ErrTenantNotFound}
	var downstreamCalled bool
	h := middleware.TenantScope(resolver)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		downstreamCalled = true
	}))

	req := httptest.NewRequest(http.MethodGet, "https://ghost.crm.local/customers", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if downstreamCalled {
		t.Fatal("downstream handler ran on a not-found host; middleware must short-circuit")
	}
	body := rec.Body.String()
	// Body MUST NOT mention the host or hint that other tenants exist —
	// secure-by-default per the issue spec.
	if strings.Contains(body, "ghost") || strings.Contains(body, "tenant") || strings.Contains(body, "subdomain") {
		t.Fatalf("404 body leaks info: %q", body)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("missing nosniff header; got %q", got)
	}
}

func TestTenantScope_TransientResolverErrorIs500(t *testing.T) {
	t.Parallel()

	resolver := &stubResolver{err: errors.New("db down")}
	h := middleware.TenantScope(resolver)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("downstream must not run on transient error")
	}))

	req := httptest.NewRequest(http.MethodGet, "https://acme.crm.local/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestTenantScope_NilResolverPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil resolver, got none")
		}
	}()
	_ = middleware.TenantScope(nil)
}
