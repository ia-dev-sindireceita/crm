package httpapi_test

// SIN-62882 — /master/tenants is gated per-route on the master-* action
// constants added in SIN-62880. These tests lock the security envelope
// at the router level:
//
//   - A tenant-role session reaching /master/tenants is denied at
//     RequireAction(ActionMasterTenantRead) with 403, AND the audited
//     Recorder captures a deny record (CA #2: "tenant gerente acessando
//     /master/* → 403 + linha em audit_log").
//   - A master-role session reaching /master/tenants is allowed; the
//     inner handler renders 200 and the Recorder captures the allow.
//
// The /m/login operator flow is the production path for master
// sessions; this test mints the master role via roledIAM (the same
// helper that backs SIN-62767) so the action gate can be exercised
// without dragging the full master-session machinery into the test.
// What is being asserted is the action-matrix wireup at the router,
// not the master-console login path.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/billing"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/authz"
	"github.com/pericles-luz/crm/internal/tenancy"
	masterweb "github.com/pericles-luz/crm/internal/web/master"
)

// ----- Stub adapters --------------------------------------------------

// Use the actual master types via alias to keep the tests verbose-but-readable.
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

// ----- Router builder -------------------------------------------------

func buildMasterRouter(t *testing.T, iamSvc httpapi.IAMService, resolver tenancy.Resolver) (http.Handler, *authzRecorder) {
	t.Helper()
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
		IAM:            iamSvc,
		TenantResolver: resolver,
		Authorizer:     audited,
		MasterTenants: httpapi.MasterTenantsRoutes{
			List:       http.HandlerFunc(h.ListTenants),
			Create:     http.HandlerFunc(h.CreateTenant),
			AssignPlan: http.HandlerFunc(h.AssignPlan),
		},
	})
	return router, rec
}

// ----- Tests ----------------------------------------------------------

// CA #2 — tenant gerente accessing /master/* is denied at RequireAction.
func TestRouter_MasterTenants_DeniesNonMasterRole(t *testing.T) {
	t.Parallel()
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": acmeID}
	store := newRoledIAM(tenantIDs)
	store.addUser("acme.crm.local", "gerente@acme.test", "pw", iam.RoleTenantGerente, uuid.New())
	resolver := &fakeResolver{byHost: tenants}

	router, rec := buildMasterRouter(t, store, resolver)
	cookie := loginCookie(t, router, "acme.crm.local", "gerente@acme.test", "pw")

	got := do(t, router, http.MethodGet, "acme.crm.local", "/master/tenants", nil, cookie)
	if got.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%q", got.Code, got.Body.String())
	}
	records := rec.snapshot()
	if len(records) == 0 {
		t.Fatalf("expected at least one audit record on deny")
	}
	denyRec := records[len(records)-1]
	if denyRec.decision.Allow {
		t.Fatalf("captured allow record on 403 path: %+v", denyRec)
	}
	if denyRec.action != iam.ActionMasterTenantRead {
		t.Fatalf("recorded action = %q, want %q", denyRec.action, iam.ActionMasterTenantRead)
	}
	if denyRec.decision.ReasonCode != iam.ReasonDeniedRBAC {
		t.Fatalf("reason_code = %q, want %q", denyRec.decision.ReasonCode, iam.ReasonDeniedRBAC)
	}
}

// Master role authenticated through the same /login form (a synthetic
// path — production uses /m/login) reaches /master/tenants because the
// action matrix puts master.tenant.read at {RoleMaster}.
func TestRouter_MasterTenants_AllowsMasterRoleGET(t *testing.T) {
	t.Parallel()
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": acmeID}
	store := newRoledIAM(tenantIDs)
	store.addUser("acme.crm.local", "master@acme.test", "pw", iam.RoleMaster, uuid.New())
	resolver := &fakeResolver{byHost: tenants}

	router, rec := buildMasterRouter(t, store, resolver)
	cookie := loginCookie(t, router, "acme.crm.local", "master@acme.test", "pw")

	got := do(t, router, http.MethodGet, "acme.crm.local", "/master/tenants", nil, cookie)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Body.String(), "Tenants") {
		t.Fatalf("body missing page title; got=%q", got.Body.String())
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

// Conditional-mount contract — when Deps.Authorizer is nil, no /master/*
// routes are mounted (defense-in-depth: master surface refuses to come
// up un-audited).
func TestRouter_MasterTenants_SkippedWhenAuthorizerNil(t *testing.T) {
	t.Parallel()
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": acmeID}
	store := newInmemIAM(tenantIDs)
	store.addUser("acme.crm.local", "alice@acme.test", "pw", uuid.New())
	resolver := &fakeResolver{byHost: tenants}

	router := httpapi.NewRouter(httpapi.Deps{
		IAM:            store,
		TenantResolver: resolver,
		// Authorizer deliberately nil.
		MasterTenants: httpapi.MasterTenantsRoutes{
			List: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		},
	})
	cookie := loginCookie(t, router, "acme.crm.local", "alice@acme.test", "pw")
	got := do(t, router, http.MethodGet, "acme.crm.local", "/master/tenants", nil, cookie)
	if got.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (route should be un-mounted without an audited Authorizer)", got.Code)
	}
}

// Sanity: each MasterTenants slot mounts independently. A wire that
// passes only List mounts GET /master/tenants but leaves POST and
// PATCH unmounted (chi answers 404/405 on the un-mounted verbs).
func TestRouter_MasterTenants_PerRouteMounting(t *testing.T) {
	t.Parallel()
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": acmeID}
	store := newRoledIAM(tenantIDs)
	store.addUser("acme.crm.local", "master@acme.test", "pw", iam.RoleMaster, uuid.New())
	resolver := &fakeResolver{byHost: tenants}

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
		IAM:            store,
		TenantResolver: resolver,
		Authorizer:     audited,
		MasterTenants: httpapi.MasterTenantsRoutes{
			List: listOnly,
			// Create and AssignPlan deliberately unset.
		},
	})
	cookie := loginCookie(t, router, "acme.crm.local", "master@acme.test", "pw")

	// GET hits the list-only handler.
	getRec := do(t, router, http.MethodGet, "acme.crm.local", "/master/tenants", nil, cookie)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%q", getRec.Code, getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), "list-only-handler") {
		t.Fatalf("GET did not hit listOnly handler: %q", getRec.Body.String())
	}

	// PATCH /master/tenants/{id}/plan is on a different path → unmounted
	// path returns 404. The action gate never runs.
	patchRec := do(t, router, http.MethodPatch, "acme.crm.local",
		"/master/tenants/"+uuid.New().String()+"/plan", nil, cookie)
	if patchRec.Code != http.StatusNotFound {
		t.Fatalf("PATCH status = %d, want 404 when AssignPlan is unset; body=%q",
			patchRec.Code, patchRec.Body.String())
	}
}

// Compile-time guard: keep errors import live (referenced via _ assignment
// to keep the import explicit for future use).
var _ = errors.New
