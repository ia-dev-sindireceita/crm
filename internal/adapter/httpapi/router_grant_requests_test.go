package httpapi_test

// SIN-65264 Leg 5b — /master/grants/requests is now served on the
// master-console host behind the master-session chain (same envelope as
// router_webmaster_test.go). Tests use the GET routes (list, show) to
// assert the gating because POST verbs would need a CSRF round-trip.
//
// The pre-relocation CA#2 deny-audit (tenant gerente → 403 at RequireAction)
// is re-homed to MasterHostOnly+RequireMasterAuth (SIN-65269 R2, tested in
// auth_test.go + master_principal_test.go). The RBAC matrix is still covered
// exhaustively in internal/iam/authorizer_test.go and authz_contract_test.go.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/authz"
)

func buildGrantRequestsRouter(t *testing.T) (http.Handler, *authzRecorder) {
	t.Helper()
	masterID := uuid.New()
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
		IAM:            newInmemIAM(nil),
		TenantResolver: &fakeResolver{},
		Authorizer:     audited,
		MasterHost:     masterConsoleTestHost,
		Master: httpapi.MasterDeps{
			Login:                      http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}),
			RequireMasterAuth:          fakeMasterAuth(masterID),
			RequireMasterMFA:           fakeMasterMFA,
			RequirePrincipalFromMaster: mastermfa.RequirePrincipalFromMaster(mastermfa.RequirePrincipalFromMasterConfig{MasterHost: masterConsoleTestHost}),
		},
		MasterTenants: httpapi.MasterTenantsRoutes{
			GrantRequestsCreate:  respondOK,
			GrantRequestsList:    respondOK,
			GrantRequestsShow:    respondOK,
			GrantRequestsApprove: respondOK,
			GrantRequestsReject:  respondOK,
		},
	})
	return router, rec
}

// TestRouter_MasterGrantRequests_OnHost_AllowMaster — on master host with
// valid master session, GET list+show reach the handler and record an allow.
func TestRouter_MasterGrantRequests_AllowMaster(t *testing.T) {
	t.Parallel()
	router, _ := buildGrantRequestsRouter(t)

	reqID := uuid.NewString()
	cases := []struct {
		name, path string
	}{
		{"list", "/master/grants/requests"},
		{"show", "/master/grants/requests/" + reqID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := masterOpReq(http.MethodGet, masterConsoleTestHost, tc.path)
			w := doReq(t, router, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "grant-requests-handler") {
				t.Fatalf("body did not reach inner handler: %q", w.Body.String())
			}
		})
	}
}

// Off-master-host (tenant host) must receive 404 before any auth runs (SecEng C2).
func TestRouter_MasterGrantRequests_OffHost_404(t *testing.T) {
	t.Parallel()
	router, rec := buildGrantRequestsRouter(t)

	r := masterOpReq(http.MethodGet, "acme.crm.local", "/master/grants/requests")
	w := doReq(t, router, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (off-host)", w.Code)
	}
	if len(rec.snapshot()) > 0 {
		t.Fatalf("authz recorder has %d entries; want 0 on off-host 404", len(rec.snapshot()))
	}
}

// Authorizer nil → surface not mounted.
func TestRouter_MasterGrantRequests_SkippedWhenAuthorizerNil(t *testing.T) {
	t.Parallel()
	masterID := uuid.New()
	router := httpapi.NewRouter(httpapi.Deps{
		IAM:            newInmemIAM(nil),
		TenantResolver: &fakeResolver{},
		// Authorizer deliberately nil.
		MasterHost: masterConsoleTestHost,
		Master: httpapi.MasterDeps{
			Login:                      http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}),
			RequireMasterAuth:          fakeMasterAuth(masterID),
			RequireMasterMFA:           fakeMasterMFA,
			RequirePrincipalFromMaster: mastermfa.RequirePrincipalFromMaster(mastermfa.RequirePrincipalFromMasterConfig{MasterHost: masterConsoleTestHost}),
		},
		MasterTenants: httpapi.MasterTenantsRoutes{
			GrantRequestsList: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		},
	})
	r := masterOpReq(http.MethodGet, masterConsoleTestHost, "/master/grants/requests")
	w := doReq(t, router, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (un-mounted without Authorizer)", w.Code)
	}
}

// Each slot mounts independently; an unset slot leaves its route as 404.
func TestRouter_MasterGrantRequests_PerSlotMounting(t *testing.T) {
	t.Parallel()
	masterID := uuid.New()
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
		IAM:            newInmemIAM(nil),
		TenantResolver: &fakeResolver{},
		Authorizer:     audited,
		MasterHost:     masterConsoleTestHost,
		Master: httpapi.MasterDeps{
			Login:                      http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}),
			RequireMasterAuth:          fakeMasterAuth(masterID),
			RequireMasterMFA:           fakeMasterMFA,
			RequirePrincipalFromMaster: mastermfa.RequirePrincipalFromMaster(mastermfa.RequirePrincipalFromMasterConfig{MasterHost: masterConsoleTestHost}),
		},
		MasterTenants: httpapi.MasterTenantsRoutes{
			GrantRequestsList: listOnly,
			// Show, Create, Approve, Reject deliberately unset.
		},
	})

	r := masterOpReq(http.MethodGet, masterConsoleTestHost, "/master/grants/requests")
	w := doReq(t, router, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", w.Code)
	}

	r2 := masterOpReq(http.MethodGet, masterConsoleTestHost, "/master/grants/requests/"+uuid.NewString())
	w2 := doReq(t, router, r2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("show status = %d, want 404 (slot unset)", w2.Code)
	}
}
