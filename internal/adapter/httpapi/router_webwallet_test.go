package httpapi_test

// SIN-63942 / UX-F5 — WebWallet mount-point integration tests.
//
// The wallet HTMX UI lives in internal/web/walletui; cmd/server's
// walletui_wire.go constructs the inner http.Handler (an
// *http.ServeMux returned by walletui.Handler.Routes) and hands it to
// httpapi.NewRouter via Deps.WebWallet. These tests pin the security
// envelope chi applies on the way in:
//
//   - GET /wallet requires Auth (302 → /login when no session).
//   - GET /wallet is gated on RequireAction(ActionTenantWalletViewLedger):
//       Gerente reaches the inner handler with a 200;
//       Atendente / Common are denied at the gate with 403.
//   - Deps.WebWallet = nil keeps every /wallet* route unmounted.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/authz"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// recordingWallet echoes each call so the test can prove RequireAuth +
// RequireAction fired before the inner handler.
type recordingWallet struct {
	calls []recordedCall
}

func (r *recordingWallet) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	_, ok := iam.PrincipalFromContext(req.Context())
	r.calls = append(r.calls, recordedCall{
		method:       req.Method,
		path:         req.URL.Path,
		hadPrincipal: ok,
	})
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func newWebWalletRouter(t *testing.T, h http.Handler, withAuthz bool) (http.Handler, *roledIAM, string) {
	t.Helper()
	const host = "acme.crm.local"
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		host: {ID: acmeID, Name: "acme", Host: host},
	}
	tenantIDs := map[string]uuid.UUID{host: acmeID}
	store := newRoledIAM(tenantIDs)
	resolver := &fakeResolver{byHost: tenants}

	deps := httpapi.Deps{
		IAM:            store,
		TenantResolver: resolver,
		WebWallet:      h,
	}
	if withAuthz {
		deps.Authorizer = authz.New(authz.Config{
			Inner:    iam.NewRBACAuthorizer(iam.RBACConfig{}),
			Recorder: &authzRecorder{},
			Sampler:  authz.NeverSample{},
		})
	}
	return httpapi.NewRouter(deps), store, host
}

func TestRouter_WebWallet_GetRequiresSession(t *testing.T) {
	t.Parallel()
	walletH := &recordingWallet{}
	h, _, host := newWebWalletRouter(t, walletH, false)
	rec := do(t, h, http.MethodGet, host, "/wallet", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302 (redirect to /login)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("Location=%q, want /login...", loc)
	}
	if len(walletH.calls) != 0 {
		t.Fatalf("inner handler called without a session: %+v", walletH.calls)
	}
}

func TestRouter_WebWallet_GerenteAllowed(t *testing.T) {
	t.Parallel()
	walletH := &recordingWallet{}
	h, store, host := newWebWalletRouter(t, walletH, true)
	store.addUser(host, "gerente@acme.test", "pw", iam.RoleTenantGerente, uuid.New())

	sess := loginCookie(t, h, host, "gerente@acme.test", "pw")
	for _, path := range []string{"/wallet", "/wallet/topup", "/wallet/ledger", "/wallet/ledger.csv"} {
		rec := do(t, h, http.MethodGet, host, path, nil, sess)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d for %q, want 200 (gerente must reach wallet); body=%q", rec.Code, path, rec.Body.String())
		}
	}
	if len(walletH.calls) != 4 {
		t.Fatalf("inner call count=%d, want 4", len(walletH.calls))
	}
	for _, c := range walletH.calls {
		if !c.hadPrincipal {
			t.Fatalf("inner handler ran without iam.Principal (RequireAuth missing): %+v", c)
		}
	}
}

func TestRouter_WebWallet_AtendenteDenied(t *testing.T) {
	t.Parallel()
	walletH := &recordingWallet{}
	h, store, host := newWebWalletRouter(t, walletH, true)
	store.addUser(host, "atendente@acme.test", "pw", iam.RoleTenantAtendente, uuid.New())

	sess := loginCookie(t, h, host, "atendente@acme.test", "pw")
	rec := do(t, h, http.MethodGet, host, "/wallet", nil, sess)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (atendente must be denied)", rec.Code)
	}
	if len(walletH.calls) != 0 {
		t.Fatalf("inner handler ran on deny path: %+v", walletH.calls)
	}
}

func TestRouter_WebWallet_CommonDenied(t *testing.T) {
	t.Parallel()
	walletH := &recordingWallet{}
	h, store, host := newWebWalletRouter(t, walletH, true)
	store.addUser(host, "common@acme.test", "pw", iam.RoleTenantCommon, uuid.New())

	sess := loginCookie(t, h, host, "common@acme.test", "pw")
	rec := do(t, h, http.MethodGet, host, "/wallet", nil, sess)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (common must be denied)", rec.Code)
	}
	if len(walletH.calls) != 0 {
		t.Fatalf("inner handler ran on deny path: %+v", walletH.calls)
	}
}

func TestRouter_WebWallet_NilDepsKeepRouteUnmounted(t *testing.T) {
	t.Parallel()
	h, store, host := newWebWalletRouter(t, nil, true)
	store.addUser(host, "gerente@acme.test", "pw", iam.RoleTenantGerente, uuid.New())

	sess := loginCookie(t, h, host, "gerente@acme.test", "pw")
	for _, path := range []string{"/wallet", "/wallet/topup", "/wallet/ledger", "/wallet/ledger.csv"} {
		rec := do(t, h, http.MethodGet, host, path, nil, sess)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status=%d for %q, want 404 (route not mounted)", rec.Code, path)
		}
	}
}
