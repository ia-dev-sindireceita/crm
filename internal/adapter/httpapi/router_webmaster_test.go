package httpapi_test

// SIN-65264 Leg 5b — /master/tenants is now served on the master-console
// host behind the master-session chain (MasterHostOnly → RequireMasterOriginCSRF
// → RequireMasterAuth → RequireMasterMFA → RequirePrincipalFromMaster →
// RequireAction → handler). Tests in this file lock that envelope:
//
//   - On-master-host with a valid master session: 200 + audited allow.
//   - Off-master-host (tenant host): 404 + no synthesis (SecEng C2, SIN-65266).
//   - MasterHost unset or Authorizer nil: surface is not mounted (404).
//   - Per-route mounting: only wired slots are reachable.
//
// The pre-relocation CA#2 deny-audit (tenant gerente → 403 + audit row via
// RequireAction) no longer applies: the relocated surface is host-pinned and
// a tenant session can never reach it. SIN-65269 R2 re-homes that audit to
// MasterHostOnly and RequireMasterAuth (see master_principal_test.go +
// auth_test.go). See project_sin65264_master_console_e2e.md for the full
// relocation rationale.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/billing"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/authz"
	masterweb "github.com/pericles-luz/crm/internal/web/master"
)

const masterConsoleTestHost = "master.crm.local"

// ----- Stub adapters --------------------------------------------------

type (
	mListResult       = masterweb.ListResult
	mListOptions      = masterweb.ListOptions
	mTenantRow        = masterweb.TenantRow
	mCreateInput      = masterweb.CreateTenantInput
	mCreateResult     = masterweb.CreateTenantResult
	mAssignPlanInput  = masterweb.AssignPlanInput
	mAssignPlanResult = masterweb.AssignPlanResult
)

type masterListerOK struct{ rows []mTenantRow }

func (s *masterListerOK) List(_ context.Context, _ mListOptions) (mListResult, error) {
	return mListResult{Tenants: s.rows, Page: 1, PageSize: 25, TotalCount: len(s.rows)}, nil
}

type masterCreatorOK struct{}

func (s *masterCreatorOK) Create(_ context.Context, in mCreateInput) (mCreateResult, error) {
	return mCreateResult{Tenant: mTenantRow{
		ID:   uuid.New(),
		Name: in.Name,
		Host: in.Host,
	}}, nil
}

type masterPlansOK struct{}

func (s *masterPlansOK) List(_ context.Context) ([]billing.Plan, error) {
	return []billing.Plan{{ID: uuid.New(), Slug: "pro", Name: "Pro", MonthlyTokenQuota: 1000}}, nil
}

type masterAssignerOK struct{}

func (s *masterAssignerOK) Assign(_ context.Context, in mAssignPlanInput) (mAssignPlanResult, error) {
	return mAssignPlanResult{Tenant: mTenantRow{ID: in.TenantID, Name: "X", PlanSlug: in.PlanSlug}}, nil
}

// ----- Fake master-session middleware ---------------------------------

// fakeMasterAuth injects a master context so handlers can read it.
// It stands in for the real RequireMasterAuth (which needs a session store)
// in tests that want to assert routing logic, not session mechanics.
func fakeMasterAuth(masterID uuid.UUID) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := mastermfa.WithMaster(r.Context(), mastermfa.Master{
				ID:    masterID,
				Email: "ops@master.test",
			})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// fakeMasterMFA is a passthrough (the test already has a verified master
// session; we are testing the routing, not the MFA bit).
func fakeMasterMFA(next http.Handler) http.Handler { return next }

// ----- Router builder -------------------------------------------------

func buildMasterOperatorRouter(t *testing.T, masterHost string) (http.Handler, *authzRecorder) {
	t.Helper()
	masterID := uuid.New()
	rec := &authzRecorder{}
	audited := authz.New(authz.Config{
		Inner:    iam.NewRBACAuthorizer(iam.RBACConfig{}),
		Recorder: rec,
		Sampler:  authz.AlwaysSample{},
	})
	h, err := masterweb.New(masterweb.Deps{
		Tenants:   &masterListerOK{rows: []mTenantRow{{ID: uuid.New(), Name: "Acme", Host: "acme.crm.local", PlanSlug: "pro", PlanName: "Pro"}}},
		Creator:   &masterCreatorOK{},
		Plans:     &masterPlansOK{},
		Assigner:  &masterAssignerOK{},
		CSRFToken: func(*http.Request) string { return "csrf-test-token" },
	})
	if err != nil {
		t.Fatalf("masterweb.New: %v", err)
	}
	router := httpapi.NewRouter(httpapi.Deps{
		IAM:            newInmemIAM(nil),
		TenantResolver: &fakeResolver{},
		Authorizer:     audited,
		MasterHost:     masterHost,
		Master: httpapi.MasterDeps{
			Login:                      http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }),
			RequireMasterAuth:          fakeMasterAuth(masterID),
			RequireMasterMFA:           fakeMasterMFA,
			RequirePrincipalFromMaster: mastermfa.RequirePrincipalFromMaster(mastermfa.RequirePrincipalFromMasterConfig{MasterHost: masterHost}),
		},
		MasterTenants: httpapi.MasterTenantsRoutes{
			List:       http.HandlerFunc(h.ListTenants),
			Create:     http.HandlerFunc(h.CreateTenant),
			AssignPlan: http.HandlerFunc(h.AssignPlan),
		},
	})
	return router, rec
}

// masterOpReq builds a request with the correct Host and an Origin header
// (CSRF-6: unsafe methods on the /master/* group go through the Origin
// gate when MasterHost is set). Safe methods (GET) are also tested here
// with a matching Origin for completeness.
func masterOpReq(method, host, path string) *http.Request {
	r, _ := http.NewRequest(method, path, nil)
	r.Host = host
	r.Header.Set("Origin", "https://"+host)
	return r
}

// ----- Tests ----------------------------------------------------------

// SecEng C2 (SIN-65266 / SIN-65264): a valid master session arriving on
// the TENANT host must receive 404 — no principal synthesis, no handler.
func TestRouter_MasterTenants_OffHost_404_NoSynthesis(t *testing.T) {
	t.Parallel()
	router, rec := buildMasterOperatorRouter(t, masterConsoleTestHost)

	// Request arrives on the tenant host (Host: acme.crm.local), not master.
	r := masterOpReq(http.MethodGet, "acme.crm.local", "/master/tenants")
	w := doReq(t, router, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (off-host must be 404, not %d)", w.Code, w.Code)
	}
	// MasterHostOnly must never reach the principal bridge — no synthesis.
	if len(rec.snapshot()) > 0 {
		t.Fatalf("authz recorder captured %d entries on off-host probe; want 0 (MasterHostOnly must stop before RequireAction)", len(rec.snapshot()))
	}
}

// On-master-host with a valid (fake) master session and MFA cleared:
// GET /master/tenants must reach the handler and return 200, and the
// audited Authorizer must record an allow for ActionMasterTenantRead.
func TestRouter_MasterTenants_OnHost_AllowsMasterRole(t *testing.T) {
	t.Parallel()
	router, rec := buildMasterOperatorRouter(t, masterConsoleTestHost)

	r := masterOpReq(http.MethodGet, masterConsoleTestHost, "/master/tenants")
	w := doReq(t, router, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Tenants") {
		t.Fatalf("body missing page title; got=%q", w.Body.String())
	}
	records := rec.snapshot()
	if len(records) == 0 {
		t.Fatalf("expected at least one audit record on allow")
	}
	last := records[len(records)-1]
	if !last.decision.Allow {
		t.Fatalf("captured deny record on 200 path: %+v", last)
	}
	if last.action != iam.ActionMasterTenantRead {
		t.Fatalf("recorded action = %q, want %q", last.action, iam.ActionMasterTenantRead)
	}
}

// When Deps.Authorizer is nil the /master/* operator surface must not be
// mounted (deny-by-default: no un-audited action gate).
func TestRouter_MasterTenants_SkippedWhenAuthorizerNil(t *testing.T) {
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
			List: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		},
	})
	r := masterOpReq(http.MethodGet, masterConsoleTestHost, "/master/tenants")
	w := doReq(t, router, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (un-mounted without an audited Authorizer)", w.Code)
	}
}

// When Deps.MasterHost is empty the /master/* surface must not be mounted.
func TestRouter_MasterTenants_SkippedWhenMasterHostUnset(t *testing.T) {
	t.Parallel()
	masterID := uuid.New()
	rec := &authzRecorder{}
	audited := authz.New(authz.Config{
		Inner:    iam.NewRBACAuthorizer(iam.RBACConfig{}),
		Recorder: rec,
		Sampler:  authz.AlwaysSample{},
	})
	router := httpapi.NewRouter(httpapi.Deps{
		IAM:            newInmemIAM(nil),
		TenantResolver: &fakeResolver{},
		Authorizer:     audited,
		// MasterHost deliberately empty.
		Master: httpapi.MasterDeps{
			Login:                      http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}),
			RequireMasterAuth:          fakeMasterAuth(masterID),
			RequireMasterMFA:           fakeMasterMFA,
			RequirePrincipalFromMaster: mastermfa.RequirePrincipalFromMaster(mastermfa.RequirePrincipalFromMasterConfig{MasterHost: ""}),
		},
		MasterTenants: httpapi.MasterTenantsRoutes{
			List: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}),
		},
	})
	r := masterOpReq(http.MethodGet, masterConsoleTestHost, "/master/tenants")
	w := doReq(t, router, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (un-mounted without MasterHost)", w.Code)
	}
}

// Each MasterTenants slot mounts independently. A wire that passes only List
// mounts GET /master/tenants but leaves POST and PATCH unmounted.
func TestRouter_MasterTenants_PerRouteMounting(t *testing.T) {
	t.Parallel()
	masterID := uuid.New()
	rec := &authzRecorder{}
	audited := authz.New(authz.Config{
		Inner:    iam.NewRBACAuthorizer(iam.RBACConfig{}),
		Recorder: rec,
		Sampler:  authz.AlwaysSample{},
	})
	listOnly := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("list-only-handler"))
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
			List: listOnly,
			// Create and AssignPlan deliberately unset.
		},
	})

	getReq := masterOpReq(http.MethodGet, masterConsoleTestHost, "/master/tenants")
	getRec := doReq(t, router, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%q", getRec.Code, getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), "list-only-handler") {
		t.Fatalf("GET did not hit listOnly handler: %q", getRec.Body.String())
	}

	// PATCH /master/tenants/{id}/plan is not mounted → 404.
	patchReq := masterOpReq(http.MethodPatch, masterConsoleTestHost, "/master/tenants/"+uuid.New().String()+"/plan")
	patchReq.Header.Set("Origin", "https://"+masterConsoleTestHost)
	patchRec := doReq(t, router, patchReq)
	if patchRec.Code != http.StatusNotFound {
		t.Fatalf("PATCH status = %d, want 404 when AssignPlan is unset; body=%q",
			patchRec.Code, patchRec.Body.String())
	}
}

// doReq is a thin wrapper over httptest.NewRecorder for tests that
// already have a built *http.Request (e.g. masterReq).
func doReq(t *testing.T, h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
