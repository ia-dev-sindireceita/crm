package httpapi_test

// SIN-65008 — WebDashboard mount-point integration tests (frontend half
// of the Dashboard / relatórios epic SIN-64963; SIN-65007 read-model).
//
// The dashboard HTMX UI lives in internal/web/dashboard; cmd/server's
// dashboard_wire.go constructs the inner http.Handler (an *http.ServeMux
// returned by dashboard.Handler.Routes) and hands it to httpapi.NewRouter
// via Deps.WebDashboard. These tests pin the security envelope chi
// applies on the way in:
//
//   - GET /dashboard requires Auth (302 → /login when no session).
//   - GET /dashboard + /dashboard/export.csv are gated on
//     RequireAction(ActionTenantContactRead): atendente / gerente reach
//     the inner handler with 200. That action is also granted to
//     tenant_common on the ADR-0090 matrix, so common reaches it too —
//     the gate is deliberately broad so the only HTTP-loginable seed user
//     (agent@acme = atendente) is not 403'd in the staging smoke;
//     tightening RBAC to gerente-only is the noted follow-up once a
//     gerente seed is smoke-loginable.
//   - Deps.WebDashboard = nil keeps both routes unmounted (chi → 404).
//
// They use a recording http.Handler in the WebDashboard slot so the
// assertions stay tied to the chi mounting (not the inner template
// rendering, which is covered by web/dashboard handler tests).

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/authz"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// recordingDashboard is the http.Handler we plug into Deps.WebDashboard.
// It echoes the method+path that reached it and records whether the
// iam.Principal was attached so the test can prove the
// RequireAuth → RequireAction → handler chain fired in order.
type recordingDashboard struct {
	calls []recordedCall
}

func (r *recordingDashboard) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	_, ok := iam.PrincipalFromContext(req.Context())
	r.calls = append(r.calls, recordedCall{
		method:       req.Method,
		path:         req.URL.Path,
		hadPrincipal: ok,
	})
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// newWebDashboardRouter wires a router with the supplied WebDashboard slot
// and (optionally) a role-aware IAMService + production RBAC authorizer.
func newWebDashboardRouter(t *testing.T, dashboardHandler http.Handler, withAuthz bool) (http.Handler, *roledIAM, string) {
	t.Helper()
	const host = "acme.crm.local"
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		host: {ID: acmeID, Name: "acme", Host: host},
	}
	tenantIDs := map[string]uuid.UUID{host: acmeID}
	store := newRoledIAM(tenantIDs)
	resolver := &fakeResolver{byHost: tenants}

	deps := httpapi.Deps{
		IAM:            store,
		TenantResolver: resolver,
		WebDashboard:   dashboardHandler,
	}
	if withAuthz {
		deps.Authorizer = authz.New(authz.Config{
			Inner:    iam.NewRBACAuthorizer(iam.RBACConfig{}),
			Recorder: &authzRecorder{},
			Sampler:  authz.NeverSample{},
		})
	}
	return httpapi.NewRouter(deps), store, host
}

// TestRouter_WebDashboard_GetRequiresSession asserts /dashboard sits
// behind middleware.Auth. With no session cookie, chi redirects to /login.
func TestRouter_WebDashboard_GetRequiresSession(t *testing.T) {
	t.Parallel()
	dashH := &recordingDashboard{}
	h, _, host := newWebDashboardRouter(t, dashH, false)
	rec := do(t, h, http.MethodGet, host, "/dashboard", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302 (redirect to /login)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("Location=%q, want /login...", loc)
	}
	if len(dashH.calls) != 0 {
		t.Fatalf("inner handler was called without a session: %+v", dashH.calls)
	}
}

// TestRouter_WebDashboard_AtendenteAllowed pins AC #1: a session minted
// with tenant_atendente reaches the inner handler on both routes with a
// 200 and an installed Principal.
func TestRouter_WebDashboard_AtendenteAllowed(t *testing.T) {
	t.Parallel()
	dashH := &recordingDashboard{}
	h, store, host := newWebDashboardRouter(t, dashH, true)
	store.addUser(host, "atendente@acme.test", "pw", iam.RoleTenantAtendente, uuid.New())

	sess := loginCookie(t, h, host, "atendente@acme.test", "pw")
	for _, path := range []string{"/dashboard", "/dashboard/export.csv"} {
		rec := do(t, h, http.MethodGet, host, path, nil, sess)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d for %q, want 200 (atendente must reach dashboard; body=%q)", rec.Code, path, rec.Body.String())
		}
	}
	if len(dashH.calls) != 2 {
		t.Fatalf("inner call count=%d, want 2 (%+v)", len(dashH.calls), dashH.calls)
	}
	for _, c := range dashH.calls {
		if !c.hadPrincipal {
			t.Fatalf("inner handler ran without iam.Principal (RequireAuth missing): %+v", c)
		}
	}
}

// TestRouter_WebDashboard_GerenteAllowed pins that gerente — the role
// superset — also reaches the dashboard.
func TestRouter_WebDashboard_GerenteAllowed(t *testing.T) {
	t.Parallel()
	dashH := &recordingDashboard{}
	h, store, host := newWebDashboardRouter(t, dashH, true)
	store.addUser(host, "gerente@acme.test", "pw", iam.RoleTenantGerente, uuid.New())

	sess := loginCookie(t, h, host, "gerente@acme.test", "pw")
	rec := do(t, h, http.MethodGet, host, "/dashboard", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (gerente inherits contact.read)", rec.Code)
	}
	if len(dashH.calls) != 1 {
		t.Fatalf("inner call count=%d, want 1", len(dashH.calls))
	}
}

// TestRouter_WebDashboard_NilDepsKeepRouteUnmounted proves the
// nil-handler branch in NewRouter: with Deps.WebDashboard unset both
// routes return 404. This is the fail-soft contract documented on
// Deps.WebDashboard.
func TestRouter_WebDashboard_NilDepsKeepRouteUnmounted(t *testing.T) {
	t.Parallel()
	h, store, host := newWebDashboardRouter(t, nil, true)
	store.addUser(host, "atendente@acme.test", "pw", iam.RoleTenantAtendente, uuid.New())

	sess := loginCookie(t, h, host, "atendente@acme.test", "pw")
	for _, path := range []string{"/dashboard", "/dashboard/export.csv"} {
		rec := do(t, h, http.MethodGet, host, path, nil, sess)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status=%d for %q, want 404 (route not mounted)", rec.Code, path)
		}
	}
}
