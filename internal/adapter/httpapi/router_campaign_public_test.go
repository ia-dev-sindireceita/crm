package httpapi_test

// SIN-62959 — the GET /c/{slug} public redirect mounts inside the
// tenanted group BUT outside the authed sub-group. These tests pin
// that envelope: tenant resolution happens before the handler runs;
// no Auth / CSRF middleware fires; a nil slot leaves the path
// unmounted.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/tenancy"
)

func newRouterWithCampaignPublic(t *testing.T, slot http.Handler) (http.Handler, *tenancy.Tenant) {
	t.Helper()
	tenant := &tenancy.Tenant{
		ID:   uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
		Name: "acme",
		Host: "acme.crm.local",
	}
	resolver := &fakeResolver{byHost: map[string]*tenancy.Tenant{tenant.Host: tenant}}
	store := newInmemIAM(map[string]uuid.UUID{tenant.Host: tenant.ID})
	r := httpapi.NewRouter(httpapi.Deps{
		IAM:               store,
		TenantResolver:    resolver,
		WebCampaignPublic: slot,
	})
	return r, tenant
}

func TestRouter_CampaignPublic_ServesWhenSlotWired(t *testing.T) {
	t.Parallel()
	called := false
	var capturedTenant *tenancy.Tenant
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		captured, err := tenancy.FromContext(r.Context())
		if err != nil {
			http.Error(w, "missing tenant: "+err.Error(), http.StatusInternalServerError)
			return
		}
		capturedTenant = captured
		if got := r.PathValue("slug"); got != "promo" {
			http.Error(w, "slug mismatch: "+got, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	h, tenant := newRouterWithCampaignPublic(t, stub)

	req := httptest.NewRequest(http.MethodGet, "/c/promo", nil)
	req.Host = "acme.crm.local"
	req.RemoteAddr = "203.0.113.10:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Fatalf("slot handler was not invoked; status=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusNoContent)
	}
	if capturedTenant == nil || capturedTenant.ID != tenant.ID {
		t.Fatalf("tenant in context = %+v, want %+v", capturedTenant, tenant)
	}
}

func TestRouter_CampaignPublic_404ForUnknownHost(t *testing.T) {
	t.Parallel()
	called := false
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	h, _ := newRouterWithCampaignPublic(t, stub)

	req := httptest.NewRequest(http.MethodGet, "/c/promo", nil)
	req.Host = "ghost.crm.local"
	req.RemoteAddr = "203.0.113.10:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if called {
		t.Fatalf("slot handler ran for unknown host — TenantScope did not gate")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestRouter_CampaignPublic_404WhenSlotNil(t *testing.T) {
	t.Parallel()
	h, _ := newRouterWithCampaignPublic(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/c/promo", nil)
	req.Host = "acme.crm.local"
	req.RemoteAddr = "203.0.113.10:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want %d (nil slot must leave the route unmounted)", rec.Code, http.StatusNotFound)
	}
}

func TestRouter_CampaignPublic_405ForNonGet(t *testing.T) {
	t.Parallel()
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h, _ := newRouterWithCampaignPublic(t, stub)

	req := httptest.NewRequest(http.MethodPost, "/c/promo", nil)
	req.Host = "acme.crm.local"
	req.RemoteAddr = "203.0.113.10:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
