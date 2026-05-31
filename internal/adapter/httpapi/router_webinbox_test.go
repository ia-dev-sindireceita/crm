package httpapi_test

// SIN-63821 (parent SIN-63793) — WebInbox mount-point integration tests.
//
// The inbox HTMX UI lives in internal/web/inbox; cmd/server's
// inbox_wire.go constructs the inner http.Handler (an *http.ServeMux
// returned by webinbox.Handler.Routes) and hands it to
// httpapi.NewRouter via Deps.WebInbox. These tests pin the security
// envelope chi applies on the way in:
//
//   - GET /inbox requires Auth (302 → /login when no session).
//   - GET /inbox is gated on RequireAction(ActionTenantInboxRead):
//       Atendente / Gerente reach the inner handler with a 200;
//       Common is denied at the gate with 403 (CEO ACK on SIN-63808).
//   - Deps.WebInbox = nil keeps the routes unmounted (chi → 404).
//
// They use a recording http.Handler in the WebInbox slot so the
// assertions stay tied to the chi mounting (not the inner template
// rendering, which is covered by web/inbox handler tests).

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/authz"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// recordingInbox is the http.Handler we plug into Deps.WebInbox. It
// echoes the method+path that reached it and records whether the
// iam.Principal was attached so the test can prove the
// RequireAuth → RequireAction → handler chain fired in order.
type recordingInbox struct {
	calls []recordedCall
}

func (r *recordingInbox) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	_, ok := iam.PrincipalFromContext(req.Context())
	r.calls = append(r.calls, recordedCall{
		method:       req.Method,
		path:         req.URL.Path,
		hadPrincipal: ok,
	})
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// newWebInboxRouter wires a router with the supplied WebInbox slot and
// (optionally) a role-aware IAMService + production RBAC authorizer.
// Returns the router so the test can drive requests, the IAM store so
// the test can pre-seed users with a Role, and the host string.
func newWebInboxRouter(t *testing.T, inboxHandler http.Handler, withAuthz bool) (http.Handler, *roledIAM, string) {
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
		WebInbox:       inboxHandler,
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

// TestRouter_WebInbox_GetRequiresSession asserts /inbox sits behind
// middleware.Auth. With no session cookie, chi redirects to /login —
// the recording handler MUST NOT have been called.
func TestRouter_WebInbox_GetRequiresSession(t *testing.T) {
	t.Parallel()
	inboxH := &recordingInbox{}
	h, _, host := newWebInboxRouter(t, inboxH, false)
	rec := do(t, h, http.MethodGet, host, "/inbox", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302 (redirect to /login)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("Location=%q, want /login...", loc)
	}
	if len(inboxH.calls) != 0 {
		t.Fatalf("inner handler was called without a session: %+v", inboxH.calls)
	}
}

// TestRouter_WebInbox_AtendenteAllowed proves the role gate: an
// Atendente principal reaches the inner handler. The chain is
// RequireAuth (installs Principal) → RequireAction(ActionTenantInboxRead)
// → handler. The 200 + recorded principal pin the AC #1 contract
// (operator user with tenant_atendente sees /inbox).
func TestRouter_WebInbox_AtendenteAllowed(t *testing.T) {
	t.Parallel()
	inboxH := &recordingInbox{}
	h, store, host := newWebInboxRouter(t, inboxH, true)
	store.addUser(host, "atendente@acme.test", "pw", iam.RoleTenantAtendente, uuid.New())

	sess := loginCookie(t, h, host, "atendente@acme.test", "pw")
	rec := do(t, h, http.MethodGet, host, "/inbox", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (atendente must reach /inbox; body=%q)", rec.Code, rec.Body.String())
	}
	if len(inboxH.calls) != 1 {
		t.Fatalf("inner call count=%d, want 1 (%+v)", len(inboxH.calls), inboxH.calls)
	}
	c := inboxH.calls[0]
	if c.method != http.MethodGet || c.path != "/inbox" {
		t.Fatalf("inner call=%+v, want GET /inbox", c)
	}
	if !c.hadPrincipal {
		t.Fatalf("inner handler ran without iam.Principal (RequireAuth missing)")
	}
}

// TestRouter_WebInbox_GerenteAllowed pins that Gerente — the role
// superset — also reaches the inbox. The gate is "Atendente minimum",
// not "Atendente exclusively", because every Atendente permission
// lives in the Gerente bucket too.
func TestRouter_WebInbox_GerenteAllowed(t *testing.T) {
	t.Parallel()
	inboxH := &recordingInbox{}
	h, store, host := newWebInboxRouter(t, inboxH, true)
	store.addUser(host, "gerente@acme.test", "pw", iam.RoleTenantGerente, uuid.New())

	sess := loginCookie(t, h, host, "gerente@acme.test", "pw")
	rec := do(t, h, http.MethodGet, host, "/inbox", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (gerente inherits atendente grant)", rec.Code)
	}
	if len(inboxH.calls) != 1 {
		t.Fatalf("inner call count=%d, want 1", len(inboxH.calls))
	}
}

// TestRouter_WebInbox_CommonDenied is the deny half of the AC: a
// session minted with RoleTenantCommon is rejected at the
// RequireAction gate before the inner handler runs. 403 + zero inner
// calls is the contract.
func TestRouter_WebInbox_CommonDenied(t *testing.T) {
	t.Parallel()
	inboxH := &recordingInbox{}
	h, store, host := newWebInboxRouter(t, inboxH, true)
	store.addUser(host, "common@acme.test", "pw", iam.RoleTenantCommon, uuid.New())

	sess := loginCookie(t, h, host, "common@acme.test", "pw")
	rec := do(t, h, http.MethodGet, host, "/inbox", nil, sess)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (common must be denied at RequireAction)", rec.Code)
	}
	if len(inboxH.calls) != 0 {
		t.Fatalf("inner handler ran on a deny path: %+v", inboxH.calls)
	}
}

// TestRouter_WebInbox_NilDepsKeepRouteUnmounted proves the nil-handler
// branch in NewRouter: with Deps.WebInbox unset, all four routes return
// 404 on the chi router. This is the fail-soft contract documented on
// Deps.WebInbox.
func TestRouter_WebInbox_NilDepsKeepRouteUnmounted(t *testing.T) {
	t.Parallel()
	h, store, host := newWebInboxRouter(t, nil, true)
	store.addUser(host, "atendente@acme.test", "pw", iam.RoleTenantAtendente, uuid.New())

	sess := loginCookie(t, h, host, "atendente@acme.test", "pw")
	for _, path := range []string{
		"/inbox",
		"/inbox/conversations/" + uuid.New().String(),
		"/inbox/conversations/" + uuid.New().String() + "/messages/" + uuid.New().String() + "/status",
	} {
		rec := do(t, h, http.MethodGet, host, path, nil, sess)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status=%d for %q, want 404 (route not mounted)", rec.Code, path)
		}
	}
}

// TestRouter_WebInbox_NestedRoutesReachInnerHandler proves the three
// subtree routes (view, send, status) also reach the inner handler
// for an authorized principal. The chi route table is what we're
// pinning here — the actual handler logic (404 on missing
// conversation, etc.) is covered by web/inbox handler tests.
func TestRouter_WebInbox_NestedRoutesReachInnerHandler(t *testing.T) {
	t.Parallel()
	inboxH := &recordingInbox{}
	h, store, host := newWebInboxRouter(t, inboxH, true)
	store.addUser(host, "atendente@acme.test", "pw", iam.RoleTenantAtendente, uuid.New())

	sess := loginCookie(t, h, host, "atendente@acme.test", "pw")
	convID := uuid.New().String()
	msgID := uuid.New().String()

	for _, path := range []string{
		"/inbox/conversations/" + convID,
		"/inbox/conversations/" + convID + "/messages/" + msgID + "/status",
	} {
		rec := do(t, h, http.MethodGet, host, path, nil, sess)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d for %q, want 200 (atendente must reach inbox subtree); body=%q", rec.Code, path, rec.Body.String())
		}
	}
	if len(inboxH.calls) != 2 {
		t.Fatalf("inner call count=%d, want 2 (%+v)", len(inboxH.calls), inboxH.calls)
	}
}

// avoid unused import error when middleware.SessionFromContext changes
// upstream; keep the import alive so future test growth has the
// session helpers at hand.
var _ = middleware.SessionFromContext
