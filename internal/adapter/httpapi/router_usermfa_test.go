package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// TestRouter_UserMFA_RoutesMountedWhenWebUserMFASet is the F11
// regression test mandated by SIN-63337 / SIN-63338. Before this PR,
// the production router never imported the
// internal/adapter/httpapi/usermfa handler — every /admin/2fa/* path
// resolved to 404 in staging even though the handler shipped with
// full tests. This test fails without the router wireup: a non-nil
// httpapi.Deps.WebUserMFA MUST cause the five routes from
// usermfa/doc.go to dispatch to the supplied handler.
//
// The smoke probe in cd-stg.yml asserts the same shape (302 for
// /admin/2fa/verify when called unauthenticated); this in-process
// regression guard catches the same regression at PR-open time
// without needing the staging stack.
func TestRouter_UserMFA_RoutesMountedWhenWebUserMFASet(t *testing.T) {
	t.Parallel()

	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": acmeID}
	store := newInmemIAM(tenantIDs)

	mfaCalled := false
	mfaHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mfaCalled = true
		w.WriteHeader(http.StatusOK)
	})

	r := httpapi.NewRouter(httpapi.Deps{
		IAM:            store,
		TenantResolver: &fakeResolver{byHost: tenants},
		WebUserMFA:     mfaHandler,
	})

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"GET /admin/2fa/setup", http.MethodGet, "/admin/2fa/setup"},
		{"POST /admin/2fa/setup", http.MethodPost, "/admin/2fa/setup"},
		{"GET /admin/2fa/verify", http.MethodGet, "/admin/2fa/verify"},
		{"POST /admin/2fa/verify", http.MethodPost, "/admin/2fa/verify"},
		{"POST /admin/2fa/regenerate", http.MethodPost, "/admin/2fa/regenerate"},
	}

	for _, tc := range cases {
		t.Run("unauth_"+tc.name, func(t *testing.T) {
			mfaCalled = false
			rec := doUserMFA(t, r, tc.method, tc.path, nil)
			// Unauthenticated request (no __Host-mfa-pending cookie) MUST
			// 302 to /login?next=<original>. The router's
			// usermfaPendingRedirect wrapper handles this case BEFORE
			// the inner handler runs; a 404 here means the wireup
			// regressed (F11).
			if rec.Code != http.StatusFound {
				t.Fatalf("status=%d, want 302 (no pending cookie -> /login redirect); F11 wireup regressed", rec.Code)
			}
			loc := rec.Header().Get("Location")
			if !strings.HasPrefix(loc, "/login?next=") {
				t.Fatalf("Location=%q does not start with /login?next=", loc)
			}
			if mfaCalled {
				t.Fatalf("inner handler invoked without pending cookie — wrapper bypass")
			}
		})

		t.Run("withPending_"+tc.name, func(t *testing.T) {
			mfaCalled = false
			cookie := &http.Cookie{Name: sessioncookie.NameTenantPending, Value: uuid.NewString()}
			rec := doUserMFA(t, r, tc.method, tc.path, []*http.Cookie{cookie})
			// With the pending cookie present the wrapper passes the
			// request through to the inner handler. The fake handler
			// returns 200 so the assertion confirms dispatch reached
			// the supplied http.Handler — i.e. the route was mounted
			// and the wrapper did NOT short-circuit.
			if !mfaCalled {
				t.Fatalf("inner handler NOT invoked with pending cookie — route not mounted (F11 regression)")
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("inner-handler status=%d, want 200 (fake returns 200)", rec.Code)
			}
		})
	}
}

// TestRouter_UserMFA_NotMountedWhenNil verifies the rollback path:
// a nil Deps.WebUserMFA keeps the routes unmounted, returning the
// tenanted-group's 404 instead of 302. This is the secure-by-default
// no-op state cmd/server falls back to when IAM_MFA_SEED_KEY is
// missing or malformed.
func TestRouter_UserMFA_NotMountedWhenNil(t *testing.T) {
	t.Parallel()

	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": acmeID}
	store := newInmemIAM(tenantIDs)

	r := httpapi.NewRouter(httpapi.Deps{
		IAM:            store,
		TenantResolver: &fakeResolver{byHost: tenants},
		WebUserMFA:     nil,
	})

	rec := doUserMFA(t, r, http.MethodGet, "/admin/2fa/verify", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (WebUserMFA nil -> route unmounted)", rec.Code)
	}
}

// TestRouter_UserMFALogin_OverridesPasswordOnly verifies that a
// non-nil Deps.WebUserMFALogin replaces the password-only POST /login
// handler with the MFA-aware wrapper. This guards the SIN-63338 swap
// from regressing into the legacy direct-session-mint behaviour.
func TestRouter_UserMFALogin_OverridesPasswordOnly(t *testing.T) {
	t.Parallel()

	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": acmeID}
	store := newInmemIAM(tenantIDs)

	mfaLoginCalled := false
	mfaLogin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mfaLoginCalled = true
		w.WriteHeader(http.StatusTeapot)
	})

	r := httpapi.NewRouter(httpapi.Deps{
		IAM:             store,
		TenantResolver:  &fakeResolver{byHost: tenants},
		WebUserMFALogin: mfaLogin,
	})

	rec := doUserMFA(t, r, http.MethodPost, "/login", nil)
	if !mfaLoginCalled {
		t.Fatalf("MFA-aware POST /login NOT invoked — Deps.WebUserMFALogin override regressed")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status=%d, want 418 (fake teapot signals override path); password-only handler.LoginPost would 401/200/302", rec.Code)
	}
}

func doUserMFA(t *testing.T, h http.Handler, method, target string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = "acme.crm.local"
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
