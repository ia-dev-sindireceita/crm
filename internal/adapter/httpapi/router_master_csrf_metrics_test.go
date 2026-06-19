package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/iam"
)

// SIN-65276 — Deps.MasterCSRFRejection must reach BOTH master Origin/Referer
// CSRF gates (the /m/* bootstrap group and the relocated /master/* operator
// surface) and fire with the correct OriginCSRFReason cause on a rejection.
// cmd/server wires this hook to a Prometheus counter; these tests prove the
// router threads the hook into both RequireMasterOriginCSRFConfig instances,
// so a wiring regression on either site fails the build rather than silently
// dropping the metric.

// csrfReasonSpy records the reasons OnReject is invoked with.
type csrfReasonSpy struct {
	reasons []mastermfa.OriginCSRFReason
}

func (s *csrfReasonSpy) hook() func(*http.Request, mastermfa.OriginCSRFReason) {
	return func(_ *http.Request, reason mastermfa.OriginCSRFReason) {
		s.reasons = append(s.reasons, reason)
	}
}

func TestRouter_MasterCSRFRejection_FiresOnMGroupGate(t *testing.T) {
	t.Parallel()

	spy := &csrfReasonSpy{}
	h := httpapi.NewRouter(httpapi.Deps{
		IAM:                 &inmemIAM{},
		TenantResolver:      &fakeResolver{},
		MasterHost:          masterRouterHost,
		MasterCSRFRejection: spy.hook(),
		Master: httpapi.MasterDeps{
			Login:             nopH,
			Logout:            nopH,
			Enroll:            nopH,
			Verify:            nopH,
			Regenerate:        nopH,
			RequireMasterAuth: passthroughMW,
			RequireMasterMFA:  passthroughMW,
		},
	})

	t.Run("forged tenant Origin → mismatch", func(t *testing.T) {
		spy.reasons = nil
		r := httptest.NewRequest(http.MethodPost, "/m/login", nil)
		r.Header.Set("Origin", "https://acme.crm.local")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if len(spy.reasons) != 1 || spy.reasons[0] != mastermfa.OriginCSRFReasonMismatch {
			t.Fatalf("OnReject reasons = %v, want [%s]", spy.reasons, mastermfa.OriginCSRFReasonMismatch)
		}
	})

	t.Run("absent Origin and Referer → missing", func(t *testing.T) {
		spy.reasons = nil
		r := httptest.NewRequest(http.MethodPost, "/m/login", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if len(spy.reasons) != 1 || spy.reasons[0] != mastermfa.OriginCSRFReasonMissing {
			t.Fatalf("OnReject reasons = %v, want [%s]", spy.reasons, mastermfa.OriginCSRFReasonMissing)
		}
	})

	t.Run("on-origin POST reaches handler, hook silent", func(t *testing.T) {
		spy.reasons = nil
		r := httptest.NewRequest(http.MethodPost, "/m/login", nil)
		r.Header.Set("Origin", "https://"+masterRouterHost)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (on-origin)", rec.Code)
		}
		if len(spy.reasons) != 0 {
			t.Fatalf("OnReject fired on accepted request: %v", spy.reasons)
		}
	})
}

func TestRouter_MasterCSRFRejection_FiresOnMasterSurfaceGate(t *testing.T) {
	t.Parallel()

	const host = "master.crm.local"
	spy := &csrfReasonSpy{}
	h := httpapi.NewRouter(httpapi.Deps{
		IAM:                 &inmemIAM{},
		TenantResolver:      &fakeResolver{},
		MasterHost:          host,
		Authorizer:          iam.NewRBACAuthorizer(iam.RBACConfig{}),
		MasterCSRFRejection: spy.hook(),
		Master: httpapi.MasterDeps{
			Login:                      nopH,
			RequireMasterAuth:          passthroughMW,
			RequireMasterMFA:           passthroughMW,
			RequirePrincipalFromMaster: passthroughMW,
		},
		MasterTenants: httpapi.MasterTenantsRoutes{
			Create: nopH,
		},
	})

	// Forged tenant-host Origin on the master host → the CSRF gate (which
	// sits before RequireMasterAuth in the chain) must 403 and fire the hook
	// with mismatch.
	r := httptest.NewRequest(http.MethodPost, "/master/tenants", nil)
	r.Host = host
	r.Header.Set("Origin", "https://acme.crm.local")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (forged Origin on /master/*)", rec.Code)
	}
	if len(spy.reasons) != 1 || spy.reasons[0] != mastermfa.OriginCSRFReasonMismatch {
		t.Fatalf("OnReject reasons = %v, want [%s]", spy.reasons, mastermfa.OriginCSRFReasonMismatch)
	}
}
