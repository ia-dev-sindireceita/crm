package httpapi_test

// SIN-66259 / Fase 4 — WebWASession mount-point integration tests.
//
// The WhatsApp non-official session provisioning HTMX UI lives in
// internal/web/wasession; cmd/server's wa_session_ui_wire.go constructs the
// inner http.Handler (an *http.ServeMux from wasession.Handler.Routes) and
// hands it to httpapi.NewRouter via Deps.WebWASession. These tests pin the
// security envelope chi applies on the way in:
//
//   - GET /settings/whatsapp-session requires Auth (302 → /login).
//   - Every route is gated on RequireAction(ActionTenantWASessionManage):
//       Gerente reaches the inner handler with a 200;
//       Atendente / Common are denied at the gate with 403.
//   - Deps.WebWASession = nil keeps every route unmounted (404).

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

// recordingWASession echoes each call so the test can prove RequireAuth +
// RequireAction fired before the inner handler.
type recordingWASession struct {
	calls []recordedCall
}

func (r *recordingWASession) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	_, ok := iam.PrincipalFromContext(req.Context())
	r.calls = append(r.calls, recordedCall{
		method:       req.Method,
		path:         req.URL.Path,
		hadPrincipal: ok,
	})
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func newWebWASessionRouter(t *testing.T, h http.Handler, withAuthz bool) (http.Handler, *roledIAM, string) {
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
		WebWASession:   h,
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

func TestRouter_WebWASession_GetRequiresSession(t *testing.T) {
	t.Parallel()
	h := &recordingWASession{}
	r, _, host := newWebWASessionRouter(t, h, false)
	rec := do(t, r, http.MethodGet, host, "/settings/whatsapp-session", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302 (redirect to /login)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("Location=%q, want /login...", loc)
	}
	if len(h.calls) != 0 {
		t.Fatalf("inner handler called without a session: %+v", h.calls)
	}
}

func TestRouter_WebWASession_GerenteAllowed(t *testing.T) {
	t.Parallel()
	h := &recordingWASession{}
	r, store, host := newWebWASessionRouter(t, h, true)
	store.addUser(host, "gerente@acme.test", "pw", iam.RoleTenantGerente, uuid.New())

	sess := loginCookie(t, r, host, "gerente@acme.test", "pw")
	rec := do(t, r, http.MethodGet, host, "/settings/whatsapp-session", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (gerente must reach provisioning); body=%q", rec.Code, rec.Body.String())
	}
	if len(h.calls) != 1 || !h.calls[0].hadPrincipal {
		t.Fatalf("inner handler missing principal / not called: %+v", h.calls)
	}
}

func TestRouter_WebWASession_AtendenteDenied(t *testing.T) {
	t.Parallel()
	h := &recordingWASession{}
	r, store, host := newWebWASessionRouter(t, h, true)
	store.addUser(host, "atendente@acme.test", "pw", iam.RoleTenantAtendente, uuid.New())

	sess := loginCookie(t, r, host, "atendente@acme.test", "pw")
	rec := do(t, r, http.MethodGet, host, "/settings/whatsapp-session", nil, sess)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (atendente must be denied)", rec.Code)
	}
	if len(h.calls) != 0 {
		t.Fatalf("inner handler ran on deny path: %+v", h.calls)
	}
}

func TestRouter_WebWASession_CommonDenied(t *testing.T) {
	t.Parallel()
	h := &recordingWASession{}
	r, store, host := newWebWASessionRouter(t, h, true)
	store.addUser(host, "common@acme.test", "pw", iam.RoleTenantCommon, uuid.New())

	sess := loginCookie(t, r, host, "common@acme.test", "pw")
	rec := do(t, r, http.MethodGet, host, "/settings/whatsapp-session", nil, sess)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (common must be denied)", rec.Code)
	}
	if len(h.calls) != 0 {
		t.Fatalf("inner handler ran on deny path: %+v", h.calls)
	}
}

func TestRouter_WebWASession_NilDepsKeepRoutesUnmounted(t *testing.T) {
	t.Parallel()
	r, store, host := newWebWASessionRouter(t, nil, true)
	store.addUser(host, "gerente@acme.test", "pw", iam.RoleTenantGerente, uuid.New())

	sess := loginCookie(t, r, host, "gerente@acme.test", "pw")
	for _, path := range []string{
		"/settings/whatsapp-session",
		"/settings/whatsapp-session/status",
	} {
		rec := do(t, r, http.MethodGet, host, path, nil, sess)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status=%d for %q, want 404 (route not mounted)", rec.Code, path)
		}
	}
}
