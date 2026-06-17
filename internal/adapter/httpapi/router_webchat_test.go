package httpapi_test

// SIN-64972 / ADR-0021 — the /widget/v1/* webchat routes mount inside
// the tenanted group BUT outside the authed sub-group. These tests pin
// that envelope: TenantScope resolves the tenant from Host before the
// handler runs (so tenancy.FromContext works), the standard cookie-CSRF
// middleware never fires on the widget POSTs (the widget brings its own
// X-Webchat-CSRF double-submit), and a nil slot leaves the paths
// unmounted.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/tenancy"
)

func newRouterWithWebChat(t *testing.T, slot http.Handler) (http.Handler, *tenancy.Tenant) {
	t.Helper()
	tenant := &tenancy.Tenant{
		ID:   uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
		Name: "acme",
		Host: "acme.crm.local",
	}
	resolver := &fakeResolver{byHost: map[string]*tenancy.Tenant{tenant.Host: tenant}}
	store := newInmemIAM(map[string]uuid.UUID{tenant.Host: tenant.ID})
	r := httpapi.NewRouter(httpapi.Deps{
		IAM:            store,
		TenantResolver: resolver,
		WebChat:        slot,
	})
	return r, tenant
}

// webchatStub records the tenant + path it saw and returns 204.
func webchatStub(called *bool, captured **tenancy.Tenant, seenPath *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		*seenPath = r.URL.Path
		tn, err := tenancy.FromContext(r.Context())
		if err != nil {
			http.Error(w, "missing tenant: "+err.Error(), http.StatusInternalServerError)
			return
		}
		*captured = tn
		w.WriteHeader(http.StatusNoContent)
	})
}

func TestRouter_WebChat_RoutesReachHandlerWithTenant(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/widget/v1/session"},
		{http.MethodPost, "/widget/v1/message"},
		{http.MethodGet, "/widget/v1/stream"},
	} {
		var called bool
		var captured *tenancy.Tenant
		var seenPath string
		h, tenant := newRouterWithWebChat(t, webchatStub(&called, &captured, &seenPath))

		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Host = "acme.crm.local"
		req.RemoteAddr = "203.0.113.10:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if !called {
			t.Fatalf("%s %s: handler not invoked; status=%d body=%q", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s %s: status=%d, want 204", tc.method, tc.path, rec.Code)
		}
		if captured == nil || captured.ID != tenant.ID {
			t.Fatalf("%s %s: tenant in context = %+v, want %+v", tc.method, tc.path, captured, tenant)
		}
		if seenPath != tc.path {
			t.Fatalf("handler saw path %q, want %q", seenPath, tc.path)
		}
	}
}

// The widget POSTs carry no cookie-CSRF token. They MUST still reach the
// handler — the cookie-CSRF middleware lives only on the authed
// sub-group, so mounting /widget/v1/message there would 403 every
// visitor message. This guards that regression.
func TestRouter_WebChat_Message_NotGatedByCookieCSRF(t *testing.T) {
	t.Parallel()
	var called bool
	var captured *tenancy.Tenant
	var seenPath string
	h, _ := newRouterWithWebChat(t, webchatStub(&called, &captured, &seenPath))

	req := httptest.NewRequest(http.MethodPost, "/widget/v1/message", strings.NewReader(`{}`))
	req.Host = "acme.crm.local"
	req.RemoteAddr = "203.0.113.10:1234"
	// No _csrf field, no CSRF cookie, no session cookie.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Fatalf("message handler blocked before running (cookie-CSRF leaked onto public route); status=%d", rec.Code)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want 204", rec.Code)
	}
}

func TestRouter_WebChat_404WhenSlotNil(t *testing.T) {
	t.Parallel()
	h, _ := newRouterWithWebChat(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/widget/v1/session", nil)
	req.Host = "acme.crm.local"
	req.RemoteAddr = "203.0.113.10:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (nil slot must leave routes unmounted)", rec.Code)
	}
}

func TestRouter_WebChat_404ForUnknownHost(t *testing.T) {
	t.Parallel()
	var called bool
	var captured *tenancy.Tenant
	var seenPath string
	h, _ := newRouterWithWebChat(t, webchatStub(&called, &captured, &seenPath))

	req := httptest.NewRequest(http.MethodPost, "/widget/v1/session", nil)
	req.Host = "ghost.crm.local"
	req.RemoteAddr = "203.0.113.10:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if called {
		t.Fatalf("handler ran for unknown host — TenantScope did not gate")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}
