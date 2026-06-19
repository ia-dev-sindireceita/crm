package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
)

// SIN-65269 CSRF-7 #4 — off-origin + positive pair for the /m/* master
// enroll-mint POST, exercised through the real NewRouter wiring (not the
// middleware in isolation). The enroll handler is the nop 200 stub from
// stubMasterDeps; what is under test is the Option-B Origin gate the
// router fronts the /m/* group with. RequireMasterAuth/MFA are
// passthroughs here so the request reaches the gate + handler.

// mintEnrollReq builds a POST /m/2fa/enroll with the given Origin (empty
// origin = header omitted).
func mintEnrollReq(origin string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/enroll", nil)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

func TestRouter_MasterEnrollMint_CSRF(t *testing.T) {
	t.Parallel()

	h := newMasterRouter(stubMasterDeps())

	t.Run("off-origin tenant POST is rejected with no mutation", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, mintEnrollReq("https://acme.crm.local"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (forged cross-tenant Origin)", rec.Code)
		}
	})

	t.Run("absent Origin and Referer is rejected (fail closed)", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, mintEnrollReq(""))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (no Origin/Referer)", rec.Code)
		}
	})

	t.Run("positive twin: canonical master Origin reaches the handler", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, mintEnrollReq("https://"+masterRouterHost))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (on-origin enroll mint)", rec.Code)
		}
	})
}

// Guard: the gate must not be mounted (and therefore must not 403) when
// MasterDeps is absent — the /m/* group is simply not registered.
func TestRouter_MasterEnrollMint_NoGateWithoutMasterDeps(t *testing.T) {
	t.Parallel()
	h := httpapi.NewRouter(httpapi.Deps{
		IAM:            &inmemIAM{},
		TenantResolver: &fakeResolver{},
		MasterHost:     masterRouterHost,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, mintEnrollReq("https://acme.crm.local"))
	if rec.Code == http.StatusForbidden {
		t.Fatalf("status = 403 but /m/* should be unmounted without MasterDeps; got %d", rec.Code)
	}
}
