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

// TestRouter_MasterTenants_CSRF covers CSRF-7 #1–3 for the /master/*
// operator surface (POST /master/tenants as the representative write route).
// The gate (RequireMasterOriginCSRF) sits before RequireMasterAuth in the
// chain; RequireMasterAuth + MFA are replaced by pass-through fakes here
// so the response code reflects only the CSRF gate's decision.
//
//  1. Valid __Host-sess-master + MFA session, Origin = tenant-host → 403,
//     handler must not be reached (no mutation).
//  2. Same but Origin AND Referer absent → 403 (fail closed).
//  3. Positive twin: Origin = master-host → reaches handler (would be 303
//     redirect-to-login because RequireMasterAuth is a no-op fake here, but
//     what matters is the CSRF gate does NOT fire a 403).
func TestRouter_MasterTenants_CSRF(t *testing.T) {
	t.Parallel()

	router, _ := buildMasterOperatorRouter(t, masterConsoleTestHost)

	masterPost := func(origin, path string) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, path, nil)
		r.Host = masterConsoleTestHost
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}

	t.Run("off-origin Origin (tenant host) is rejected with 403", func(t *testing.T) {
		t.Parallel()
		r := masterPost("https://acme.crm.local", "/master/tenants")
		w := doReq(t, router, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("CSRF-7 #1: status = %d, want 403 (forged cross-tenant Origin)", w.Code)
		}
	})

	t.Run("absent Origin and Referer is rejected with 403 (fail closed)", func(t *testing.T) {
		t.Parallel()
		r := masterPost("", "/master/tenants")
		w := doReq(t, router, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("CSRF-7 #2: status = %d, want 403 (both headers absent)", w.Code)
		}
	})

	t.Run("canonical master-host Origin is not blocked by CSRF gate", func(t *testing.T) {
		t.Parallel()
		r := masterPost("https://"+masterConsoleTestHost, "/master/tenants")
		w := doReq(t, router, r)
		if w.Code == http.StatusForbidden {
			t.Fatalf("CSRF-7 #3: status = 403, CSRF gate must not block canonical master-host Origin; body=%q", w.Body.String())
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
