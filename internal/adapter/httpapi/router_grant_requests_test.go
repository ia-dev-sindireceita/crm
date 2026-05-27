package httpapi_test

// SIN-63605 — router-level tests for the /master/grants/requests
// surface. Each MasterTenants.GrantRequests* slot mounts behind a
// per-route RequireAction gate; tests use the GET routes (list,
// show) to assert the gating because POST verbs in the router test
// harness would additionally need a real CSRF token round-trip. The
// RBAC matrix for the three new actions is covered exhaustively in
// internal/iam/authorizer_test.go and
// internal/adapter/httpapi/authz_contract_test.go.

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

func buildGrantRequestsRouter(t *testing.T, withRole iam.Role) (http.Handler, *authzRecorder, string) {
	t.Helper()
	host := "acme.crm.local"
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{host: {ID: acmeID, Name: "acme", Host: host}}
	tenantIDs := map[string]uuid.UUID{host: acmeID}
	store := newRoledIAM(tenantIDs)
	store.addUser(host, "u@acme.test", "pw", withRole, uuid.New())
	resolver := &fakeResolver{byHost: tenants}

	rec := &authzRecorder{}
	audited := authz.New(authz.Config{
		Inner:    iam.NewRBACAuthorizer(iam.RBACConfig{}),
		Recorder: rec,
		Sampler:  authz.AlwaysSample{},
	})
	respondOK := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("grant-requests-handler"))
	})
	router := httpapi.NewRouter(httpapi.Deps{
		IAM:            store,
		TenantResolver: resolver,
		Authorizer:     audited,
		MasterTenants: httpapi.MasterTenantsRoutes{
			GrantRequestsCreate:  respondOK,
			GrantRequestsList:    respondOK,
			GrantRequestsShow:    respondOK,
			GrantRequestsApprove: respondOK,
			GrantRequestsReject:  respondOK,
		},
	})
	return router, rec, host
}

// TestRouter_MasterGrantRequests_DenyTenantGerente exercises the GET
// routes (list + show) — these reach the RequireAction gate without
// CSRF interference and produce a clean 403 on a non-master role.
// POST routes are covered by the matrix tests in iam + authz_contract
// _test.go.
func TestRouter_MasterGrantRequests_DenyTenantGerente(t *testing.T) {
	t.Parallel()
	router, rec, host := buildGrantRequestsRouter(t, iam.RoleTenantGerente)
	cookie := loginCookie(t, router, host, "u@acme.test", "pw")

	reqID := uuid.NewString()
	cases := []struct {
		name, method, path string
		wantAction         iam.Action
	}{
		{"list", http.MethodGet, "/master/grants/requests", iam.ActionMasterGrantRequestApprove},
		{"show", http.MethodGet, "/master/grants/requests/" + reqID, iam.ActionMasterGrantRequestApprove},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := do(t, router, tc.method, host, tc.path, nil, cookie)
			if got.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%q", got.Code, got.Body.String())
			}
			records := rec.snapshot()
			if len(records) == 0 {
				t.Fatalf("no audit records captured")
			}
			last := records[len(records)-1]
			if last.action != tc.wantAction {
				t.Fatalf("captured action = %q, want %q", last.action, tc.wantAction)
			}
			if last.decision.Allow {
				t.Fatalf("captured allow on 403 path: %+v", last)
			}
			if last.decision.ReasonCode != iam.ReasonDeniedRBAC {
				t.Fatalf("reason = %q, want %q", last.decision.ReasonCode, iam.ReasonDeniedRBAC)
			}
		})
	}
}

func TestRouter_MasterGrantRequests_AllowMaster(t *testing.T) {
	t.Parallel()
	router, _, host := buildGrantRequestsRouter(t, iam.RoleMaster)
	cookie := loginCookie(t, router, host, "u@acme.test", "pw")

	reqID := uuid.NewString()
	cases := []struct {
		name, method, path string
	}{
		{"list", http.MethodGet, "/master/grants/requests"},
		{"show", http.MethodGet, "/master/grants/requests/" + reqID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := do(t, router, tc.method, host, tc.path, nil, cookie)
			if got.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", got.Code, got.Body.String())
			}
			if !strings.Contains(got.Body.String(), "grant-requests-handler") {
				t.Fatalf("body did not reach inner handler: %q", got.Body.String())
			}
		})
	}
}

// TestRouter_MasterGrantRequests_SkippedWhenAuthorizerNil — fail-closed
// behaviour: a deploy without an audited Authorizer must NOT mount the
// 4-eyes surface (defense in depth — the surface is master-only and
// must not run un-audited).
func TestRouter_MasterGrantRequests_SkippedWhenAuthorizerNil(t *testing.T) {
	t.Parallel()
	host := "acme.crm.local"
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{host: {ID: acmeID, Name: "acme", Host: host}}
	tenantIDs := map[string]uuid.UUID{host: acmeID}
	store := newInmemIAM(tenantIDs)
	store.addUser(host, "alice@acme.test", "pw", uuid.New())
	resolver := &fakeResolver{byHost: tenants}

	router := httpapi.NewRouter(httpapi.Deps{
		IAM:            store,
		TenantResolver: resolver,
		// Authorizer deliberately nil.
		MasterTenants: httpapi.MasterTenantsRoutes{
			GrantRequestsList: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		},
	})
	cookie := loginCookie(t, router, host, "alice@acme.test", "pw")
	got := do(t, router, http.MethodGet, host, "/master/grants/requests", nil, cookie)
	if got.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (route should be un-mounted without an audited Authorizer)", got.Code)
	}
}

// TestRouter_MasterGrantRequests_PerSlotMounting — each slot mounts
// independently; an unset slot leaves its route as a 404.
func TestRouter_MasterGrantRequests_PerSlotMounting(t *testing.T) {
	t.Parallel()
	host := "acme.crm.local"
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{host: {ID: acmeID, Name: "acme", Host: host}}
	tenantIDs := map[string]uuid.UUID{host: acmeID}
	store := newRoledIAM(tenantIDs)
	store.addUser(host, "m@acme.test", "pw", iam.RoleMaster, uuid.New())
	resolver := &fakeResolver{byHost: tenants}

	audited := authz.New(authz.Config{
		Inner:    iam.NewRBACAuthorizer(iam.RBACConfig{}),
		Recorder: &authzRecorder{},
		Sampler:  authz.AlwaysSample{},
	})
	listOnly := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("list-only"))
	})
	router := httpapi.NewRouter(httpapi.Deps{
		IAM:            store,
		TenantResolver: resolver,
		Authorizer:     audited,
		MasterTenants: httpapi.MasterTenantsRoutes{
			GrantRequestsList: listOnly,
			// Show, Create, Approve, Reject deliberately unset.
		},
	})
	cookie := loginCookie(t, router, host, "m@acme.test", "pw")

	got := do(t, router, http.MethodGet, host, "/master/grants/requests", nil, cookie)
	if got.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", got.Code)
	}

	reqID := uuid.NewString()
	if rec := do(t, router, http.MethodGet, host, "/master/grants/requests/"+reqID, nil, cookie); rec.Code != http.StatusNotFound {
		t.Fatalf("show status = %d, want 404 (slot unset)", rec.Code)
	}
}
