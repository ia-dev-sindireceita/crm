package httpapi_test

// SIN-65289 — /master/* 500 "csrf token missing" regression lock.
//
// Bug: SIN-65264 Leg 5b relocated the /master/* operator surface onto the
// master-host chain
//
//	MasterHostOnly → RequireMasterOriginCSRF → RequireMasterAuth →
//	RequireMasterMFA → RequirePrincipalFromMaster → RequireAction → handler
//
// which installs NO tenant session. The wire fed masterweb its CSRF token
// via csrfTokenFromSessionContext (the *tenant* session reader), so on the
// master chain that returned "" and every GET /master/* that renders a
// form 500'd with "csrf token missing" in staging — while the SIN-65264
// e2e stubbed CSRFToken and rendered 200, masking the bug (4 sequential
// post-deploy bugs = the e2e never drove the real CSRF provider through
// the real chain).
//
// These tests drive the REAL router chain with the REAL master CSRF
// provider (mastermfa.CSRFTokenFromContext) — NOT a stub — and assert
// 200 + the operator-bound token actually rendered into the form. Wire
// the tenant provider here instead and every assertion flips to 500,
// reproducing staging. This is the regression that would have caught it.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/authz"
	masterweb "github.com/pericles-luz/crm/internal/web/master"
)

// ----- Stub ports for the grants / grant-requests / detail surfaces ---

type masterGrantsPortStub struct{}

func (masterGrantsPortStub) IssueGrant(context.Context, masterweb.IssueGrantInput) (masterweb.IssueGrantResult, error) {
	return masterweb.IssueGrantResult{}, nil
}
func (masterGrantsPortStub) RevokeGrant(context.Context, masterweb.RevokeGrantInput) error {
	return nil
}
func (masterGrantsPortStub) ListGrants(context.Context, uuid.UUID) ([]masterweb.GrantRow, error) {
	return nil, nil
}

type masterGrantReqPortStub struct{}

func (masterGrantReqPortStub) CreateGrantRequest(context.Context, masterweb.CreateGrantRequestInput) (masterweb.GrantRequest, error) {
	return masterweb.GrantRequest{}, nil
}
func (masterGrantReqPortStub) ListAwaitingRequests(context.Context) ([]masterweb.GrantRequest, error) {
	return nil, nil
}
func (masterGrantReqPortStub) GetGrantRequest(context.Context, uuid.UUID) (masterweb.GrantRequest, error) {
	return masterweb.GrantRequest{}, masterweb.ErrGrantRequestNotFound
}
func (masterGrantReqPortStub) ApproveGrantRequest(context.Context, masterweb.DecideGrantRequestInput) (masterweb.GrantRow, error) {
	return masterweb.GrantRow{}, nil
}
func (masterGrantReqPortStub) RejectGrantRequest(context.Context, masterweb.DecideGrantRequestInput) error {
	return nil
}

type masterTenantDetailStub struct{ id uuid.UUID }

func (s masterTenantDetailStub) GetDetail(_ context.Context, id uuid.UUID) (masterweb.TenantDetail, error) {
	return masterweb.TenantDetail{Tenant: mTenantRow{ID: id, Name: "Acme", Host: "acme.crm.local", PlanSlug: "pro", PlanName: "Pro"}}, nil
}

// ----- Router builder that wires the REAL master CSRF provider ---------

// buildRealCSRFMasterRouter builds the full /master/* surface (tenants +
// grants + grant-requests + detail) behind the real master chain, wiring
// masterweb.Deps.CSRFToken = mastermfa.CSRFTokenFromContext exactly as
// cmd/server/master_tenants_wire.go does after SIN-65289. fakeMasterAuth
// installs a master with a known ID so the test can recompute the exact
// token the provider derives and assert it round-trips into the form.
func buildRealCSRFMasterRouter(t *testing.T) (router http.Handler, masterID, tenantID uuid.UUID) {
	t.Helper()
	masterID = uuid.New()
	tenantID = uuid.New()
	rec := &authzRecorder{}
	audited := authz.New(authz.Config{
		Inner:    iam.NewRBACAuthorizer(iam.RBACConfig{}),
		Recorder: rec,
		Sampler:  authz.AlwaysSample{},
	})
	h, err := masterweb.New(masterweb.Deps{
		Tenants:       &masterListerOK{rows: []mTenantRow{{ID: tenantID, Name: "Acme", Host: "acme.crm.local", PlanSlug: "pro", PlanName: "Pro"}}},
		Creator:       &masterCreatorOK{},
		Plans:         &masterPlansOK{},
		Assigner:      &masterAssignerOK{},
		Grants:        masterGrantsPortStub{},
		GrantRequests: masterGrantReqPortStub{},
		TenantDetail:  masterTenantDetailStub{id: tenantID},
		// THE FIX UNDER TEST — real master-aware provider, not a stub.
		CSRFToken: mastermfa.CSRFTokenFromContext,
	})
	if err != nil {
		t.Fatalf("masterweb.New: %v", err)
	}
	router = httpapi.NewRouter(httpapi.Deps{
		IAM:            newInmemIAM(nil),
		TenantResolver: &fakeResolver{},
		Authorizer:     audited,
		MasterHost:     masterConsoleTestHost,
		Master: httpapi.MasterDeps{
			Login:                      http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }),
			RequireMasterAuth:          fakeMasterAuth(masterID),
			RequireMasterMFA:           fakeMasterMFA,
			RequirePrincipalFromMaster: mastermfa.RequirePrincipalFromMaster(mastermfa.RequirePrincipalFromMasterConfig{MasterHost: masterConsoleTestHost}),
		},
		MasterTenants: httpapi.MasterTenantsRoutes{
			List:              http.HandlerFunc(h.ListTenants),
			Create:            http.HandlerFunc(h.CreateTenant),
			AssignPlan:        http.HandlerFunc(h.AssignPlan),
			Detail:            http.HandlerFunc(h.ShowTenantDetail),
			GrantsNew:         http.HandlerFunc(h.ShowGrantsForm),
			GrantRequestsList: http.HandlerFunc(h.ListGrantRequests),
		},
	})
	return router, masterID, tenantID
}

// expectedMasterToken recomputes the token the real provider derives for
// masterID, so the body assertions prove the value flowed end-to-end
// through the chain rather than matching any non-empty placeholder.
func expectedMasterToken(t *testing.T, masterID uuid.UUID) string {
	t.Helper()
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: masterID}))
	tok := mastermfa.CSRFTokenFromContext(r)
	if tok == "" {
		t.Fatal("provider returned empty token for a master in context")
	}
	return tok
}

func assertOKWithToken(t *testing.T, router http.Handler, path, wantToken string) {
	t.Helper()
	w := doReq(t, router, masterOpReq(http.MethodGet, masterConsoleTestHost, path))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (the SIN-65289 500 regression); body=%q", path, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), wantToken) {
		t.Fatalf("GET %s body missing the real master CSRF token %q; body=%q", path, wantToken, w.Body.String())
	}
}

// GET /master/tenants — the headline staging 500.
func TestRouter_MasterCSRF_ListTenants_RealProvider_200(t *testing.T) {
	t.Parallel()
	router, masterID, _ := buildRealCSRFMasterRouter(t)
	assertOKWithToken(t, router, "/master/tenants", expectedMasterToken(t, masterID))
}

// GET /master/grants/requests — the second route the ticket names; it
// shares the same handler + CSRFToken provider as ListTenants.
func TestRouter_MasterCSRF_GrantRequestsList_RealProvider_200(t *testing.T) {
	t.Parallel()
	router, masterID, _ := buildRealCSRFMasterRouter(t)
	assertOKWithToken(t, router, "/master/grants/requests", expectedMasterToken(t, masterID))
}

// GET /master/tenants/{id} — detail page, same chain (ticket scope #3).
func TestRouter_MasterCSRF_TenantDetail_RealProvider_200(t *testing.T) {
	t.Parallel()
	router, masterID, tenantID := buildRealCSRFMasterRouter(t)
	assertOKWithToken(t, router, "/master/tenants/"+tenantID.String(), expectedMasterToken(t, masterID))
}

// GET /master/tenants/{id}/grants/new — grants form, same chain (scope #3).
func TestRouter_MasterCSRF_GrantsNew_RealProvider_200(t *testing.T) {
	t.Parallel()
	router, masterID, tenantID := buildRealCSRFMasterRouter(t)
	assertOKWithToken(t, router, "/master/tenants/"+tenantID.String()+"/grants/new", expectedMasterToken(t, masterID))
}
