package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
)

// nopH is a handler that always returns 200 OK.
var nopH = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
})

// passthroughMW is a middleware that calls next unchanged.
func passthroughMW(next http.Handler) http.Handler { return next }

// recordMW returns a middleware that appends name to *calls then calls next.
func recordMW(name string, calls *[]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*calls = append(*calls, name)
			next.ServeHTTP(w, r)
		})
	}
}

// stubMasterDeps builds a MasterDeps with nop handlers and passthrough
// middleware. Individual tests override only what they need.
func stubMasterDeps() httpapi.MasterDeps {
	return httpapi.MasterDeps{
		Login:             nopH,
		Logout:            nopH,
		Enroll:            nopH,
		Verify:            nopH,
		Regenerate:        nopH,
		RequireMasterAuth: passthroughMW,
		RequireMasterMFA:  passthroughMW,
	}
}

// newMasterRouter returns a router with master deps wired. It reuses
// the fakeResolver and inmemIAM already defined in router_test.go
// (same test package).
func newMasterRouter(md httpapi.MasterDeps) http.Handler {
	return httpapi.NewRouter(httpapi.Deps{
		IAM:            &inmemIAM{},
		TenantResolver: &fakeResolver{},
		Master:         md,
	})
}

func TestMasterRoutes_LoginAndLogoutMounted(t *testing.T) {
	t.Parallel()

	var authCalls []string
	md := stubMasterDeps()
	md.RequireMasterAuth = recordMW("auth", &authCalls)

	h := newMasterRouter(md)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/m/login"},
		{http.MethodPost, "/m/login"},
		{http.MethodGet, "/m/logout"},
	} {
		authCalls = nil
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s: status = %d, want 200", tc.method, tc.path, rec.Code)
		}
		if len(authCalls) != 0 {
			t.Errorf("%s %s: RequireMasterAuth called for bootstrap route (want 0 calls)", tc.method, tc.path)
		}
	}
}

func TestMasterRoutes_VerifyBehindAuthNotMFA(t *testing.T) {
	t.Parallel()

	var authCalls, mfaCalls []string
	md := stubMasterDeps()
	md.RequireMasterAuth = recordMW("auth", &authCalls)
	md.RequireMasterMFA = recordMW("mfa", &mfaCalls)

	h := newMasterRouter(md)

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		authCalls = nil
		mfaCalls = nil
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/m/2fa/verify", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s /m/2fa/verify: status = %d, want 200", method, rec.Code)
		}
		if len(authCalls) == 0 {
			t.Errorf("%s /m/2fa/verify: RequireMasterAuth not called", method)
		}
		if len(mfaCalls) != 0 {
			t.Errorf("%s /m/2fa/verify: RequireMasterMFA called (must NOT be)", method)
		}
	}
}

func TestMasterRoutes_MFAGatedRoutesBehindBothMiddlewares(t *testing.T) {
	t.Parallel()

	cases := []struct{ method, path string }{
		{http.MethodGet, "/m/2fa/enroll"},
		{http.MethodPost, "/m/2fa/enroll"},
		{http.MethodPost, "/m/2fa/recovery/regenerate"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			var order []string
			md := stubMasterDeps()
			md.RequireMasterAuth = recordMW("auth", &order)
			md.RequireMasterMFA = recordMW("mfa", &order)

			h := newMasterRouter(md)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if len(order) < 2 || order[0] != "auth" || order[1] != "mfa" {
				t.Fatalf("middleware order = %v, want [auth mfa]", order)
			}
		})
	}
}

func TestMasterRoutes_SkippedWhenDepsNil(t *testing.T) {
	t.Parallel()

	h := httpapi.NewRouter(httpapi.Deps{
		IAM:            &inmemIAM{},
		TenantResolver: &fakeResolver{},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/m/login", nil))
	if rec.Code == http.StatusOK {
		t.Fatal("/m/login should not be mounted when MasterDeps is zero")
	}
}

// TestMasterConvention_NonBootstrapRoutesBehindMFA uses chi.Walk to
// enumerate every /m/* route and asserts RequireMasterMFA runs on all
// non-bootstrap routes (i.e. everything except /m/login, /m/logout, and
// /m/2fa/verify).
func TestMasterConvention_NonBootstrapRoutesBehindMFA(t *testing.T) {
	t.Parallel()

	// Routes that intentionally bypass RequireMasterMFA.
	noMFA := map[string]bool{
		"/m/login":      true,
		"/m/logout":     true,
		"/m/2fa/verify": true,
	}

	var mfaCalls []string
	md := stubMasterDeps()
	md.RequireMasterMFA = recordMW("mfa", &mfaCalls)

	h := newMasterRouter(md)

	chiRouter, ok := h.(chi.Router)
	if !ok {
		t.Skip("router is not a chi.Router — cannot Walk")
	}

	var errs []string
	_ = chi.Walk(chiRouter, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/m/") {
			return nil
		}
		if noMFA[route] {
			return nil
		}

		mfaCalls = nil
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, route, nil))
		if len(mfaCalls) == 0 {
			errs = append(errs, method+" "+route+": RequireMasterMFA not called")
		}
		return nil
	})

	for _, e := range errs {
		t.Error(e)
	}
}
