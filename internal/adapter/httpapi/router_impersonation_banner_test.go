package httpapi_test

// SIN-65368 / SIN-65369 — regression for the impersonation banner that
// never rendered on /master/* pages.
//
// Root cause (origin/main): ImpersonationFromSession was wired inline on
// Start/End/Feed only, so it never ran on the operator PAGE routes
// (GET /master/tenants, /master/tenants/{id}, /master/grants/requests …).
// middleware.ActiveImpersonation(ctx) was therefore always nil on those
// pages and the banner's {{with .ActiveImpersonation}} guard silently
// suppressed the red "IMPERSONANDO" banner.
//
// The fix mounts FromSession as a router-level wrapper on every /master/*
// route EXCEPT End (the design the ImpersonationRoutes godoc documents).
// These tests drive the REAL middleware chain through NewRouter — real
// RequirePrincipalFromMaster (which seeds the iam.Session, SIN-65321), real
// ImpersonationFromSession over in-memory ports, and the real masterweb
// page handler — and assert:
//
//  1. GET /master/tenants with an active envelope renders the banner.
//  2. POST /master/impersonation/end with an EXPIRED envelope still reaches
//     the End handler (FromSession is NOT on End — it would have 303'd the
//     expired envelope to /master/tenants?expired=1 before the handler ran).

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/iam/authz"
	"github.com/pericles-luz/crm/internal/iam/impersonation"
	"github.com/pericles-luz/crm/internal/tenancy"
	masterweb "github.com/pericles-luz/crm/internal/web/master"
)

// ----- in-memory impersonation ports ----------------------------------

// bannerImpRepo is the in-memory impersonation.Repo: ActiveForSession
// returns the single configured envelope (or ErrNoActiveImpersonation when
// nil). End records that it was called so the (unused-in-these-tests) write
// path is observable.
type bannerImpRepo struct {
	active  *impersonation.Session
	endHits int
}

func (r *bannerImpRepo) Start(context.Context, impersonation.StartInput) (*impersonation.Session, error) {
	return nil, impersonation.ErrNoActiveImpersonation
}

func (r *bannerImpRepo) ActiveForSession(context.Context, uuid.UUID) (*impersonation.Session, error) {
	if r.active == nil {
		return nil, impersonation.ErrNoActiveImpersonation
	}
	return r.active, nil
}

func (r *bannerImpRepo) End(context.Context, uuid.UUID, uuid.UUID, string, time.Time) error {
	r.endHits++
	return nil
}

func (r *bannerImpRepo) ListAuditByCorrelation(context.Context, uuid.UUID, int) ([]audit.SecurityRow, error) {
	return nil, nil
}

// bannerMasterChecker satisfies middleware.MasterChecker — the operator is
// always a master in these tests.
type bannerMasterChecker struct{}

func (bannerMasterChecker) IsMaster(context.Context, uuid.UUID) (bool, error) { return true, nil }

// bannerByIDResolver satisfies tenancy.ByIDResolver for both FromSession
// (step 6 tenant resolution) and the banner view-model.
type bannerByIDResolver struct{ t *tenancy.Tenant }

func (r bannerByIDResolver) ResolveByID(context.Context, uuid.UUID) (*tenancy.Tenant, error) {
	return r.t, nil
}

// bannerNoopAudit satisfies audit.SplitLogger — these tests assert routing,
// not audit persistence.
type bannerNoopAudit struct{}

func (bannerNoopAudit) WriteSecurity(context.Context, audit.SecurityAuditEvent) error { return nil }
func (bannerNoopAudit) WriteData(context.Context, audit.DataAuditEvent) error         { return nil }

// ----- harness --------------------------------------------------------

var bannerFixedClock = func() time.Time { return time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC) }

const bannerMasterSessionCookie = "11111111-aaaa-4aaa-8aaa-111111111111"

// buildBannerRouter wires NewRouter with the real RequirePrincipalFromMaster
// + the real ImpersonationFromSession over in-memory ports, the real
// masterweb List handler, and an End recording stub. The returned envelope
// (if any) is what bannerImpRepo.ActiveForSession serves.
func buildBannerRouter(t *testing.T, envelope *impersonation.Session) (http.Handler, *bannerImpRepo) {
	t.Helper()
	masterID := uuid.New()

	rec := &authzRecorder{}
	audited := authz.New(authz.Config{
		Inner:    iam.NewRBACAuthorizer(iam.RBACConfig{}),
		Recorder: rec,
		Sampler:  authz.AlwaysSample{},
	})

	targetTenant := &tenancy.Tenant{ID: uuid.New(), Name: "Acme", Host: "acme.crm.local"}
	resolver := bannerByIDResolver{t: targetTenant}

	page, err := masterweb.New(masterweb.Deps{
		Tenants:         &masterListerOK{rows: []mTenantRow{{ID: targetTenant.ID, Name: "Acme", Host: "acme.crm.local", PlanSlug: "pro", PlanName: "Pro"}}},
		Creator:         &masterCreatorOK{},
		Plans:           &masterPlansOK{},
		Assigner:        &masterAssignerOK{},
		CSRFToken:       func(*http.Request) string { return "csrf-test-token" },
		TenantsResolver: resolver,
		Clock:           bannerFixedClock,
	})
	if err != nil {
		t.Fatalf("masterweb.New: %v", err)
	}

	repo := &bannerImpRepo{active: envelope}
	fromSession := middleware.ImpersonationFromSession(
		bannerMasterChecker{}, resolver, repo, bannerNoopAudit{}, bannerFixedClock, nil,
	)

	endReached := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("end-handler-reached"))
	})

	router := httpapi.NewRouter(httpapi.Deps{
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
			List: http.HandlerFunc(page.ListTenants),
		},
		Impersonation: httpapi.ImpersonationRoutes{
			End:         endReached,
			FromSession: fromSession,
		},
	})
	return router, repo
}

// bannerEnvelope returns an active envelope keyed to the master cookie used
// by bannerReq, expiring at the supplied instant.
func bannerEnvelope(expiresAt time.Time) *impersonation.Session {
	return &impersonation.Session{
		ID:              uuid.New(),
		MasterUserID:    uuid.New(),
		MasterSessionID: uuid.MustParse(bannerMasterSessionCookie),
		TargetTenantID:  uuid.New(),
		Reason:          "support ticket #42",
		StartedAt:       bannerFixedClock().Add(-10 * time.Minute),
		ExpiresAt:       expiresAt,
	}
}

func bannerReq(method, path string) *http.Request {
	r, _ := http.NewRequest(method, path, nil)
	r.Host = masterConsoleTestHost
	r.Header.Set("Origin", "https://"+masterConsoleTestHost)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: bannerMasterSessionCookie})
	return r
}

// ----- tests ----------------------------------------------------------

// The fix: GET /master/tenants now runs FromSession (via moImp), so the
// active envelope reaches the handler and the banner renders. Before the
// fix FromSession was inline on Start/Feed only and never ran on this page
// route, so the banner was suppressed.
func TestRouter_ImpersonationBanner_RendersOnTenantsPage(t *testing.T) {
	t.Parallel()
	router, _ := buildBannerRouter(t, bannerEnvelope(bannerFixedClock().Add(1*time.Hour)))

	w := doReq(t, router, bannerReq(http.MethodGet, "/master/tenants"))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /master/tenants status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `data-impersonation-banner="true"`) {
		t.Fatalf("impersonation banner not rendered on /master/tenants — FromSession did not run on the page route.\nbody=%q", body)
	}
	if !strings.Contains(body, "shell__impersonation-banner") {
		t.Fatalf("banner aside class missing; body=%q", body)
	}
}

// Negative control: with NO active envelope the page renders without the
// banner (proves the assertion above is not a false positive from static
// markup).
func TestRouter_ImpersonationBanner_AbsentWhenNoEnvelope(t *testing.T) {
	t.Parallel()
	router, _ := buildBannerRouter(t, nil)

	w := doReq(t, router, bannerReq(http.MethodGet, "/master/tenants"))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /master/tenants status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `data-impersonation-banner="true"`) {
		t.Fatalf("banner rendered with no active envelope; body=%q", w.Body.String())
	}
}

// End must stay OUT of the FromSession wrapper: an operator whose envelope
// already expired must still be able to POST /master/impersonation/end and
// reach the handler. If End were wrapped, FromSession's expiry branch would
// 303-redirect to /master/tenants?expired=1 and the handler would never run.
func TestRouter_ImpersonationEnd_NotBehindFromSession_ExpiredEnvelope(t *testing.T) {
	t.Parallel()
	router, _ := buildBannerRouter(t, bannerEnvelope(bannerFixedClock().Add(-1*time.Hour)))

	w := doReq(t, router, bannerReq(http.MethodPost, "/master/impersonation/end"))
	if w.Code == http.StatusSeeOther {
		t.Fatalf("POST /master/impersonation/end was 303-redirected (FromSession ran on End); Location=%q", w.Header().Get("Location"))
	}
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "end-handler-reached") {
		t.Fatalf("End handler not reached: status=%d body=%q", w.Code, w.Body.String())
	}
}
