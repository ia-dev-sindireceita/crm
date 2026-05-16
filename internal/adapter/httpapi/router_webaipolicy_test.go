package httpapi_test

// SIN-62906 — WebAIPolicy mount-point integration tests.
//
// The HTMX admin UI lives in internal/web/aipolicy; cmd/server
// constructs the inner mux and hands it to httpapi.NewRouter via
// Deps.WebAIPolicy. These tests pin the security envelope chi
// applies on the way in:
//
//   - No session → 302 to /login (Auth gate fires).
//   - Session with the wrong role + Authorizer wired → 403 from
//     RequireAction(ActionTenantAIPolicyWrite).
//   - Session as gerente + Authorizer wired → reaches the inner
//     handler with iam.Principal in context.
//
// The inner handler is a recording http.Handler so the assertions
// stay tied to the chi mounting (the web/aipolicy package covers
// the rendering exhaustively).

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// recordingAIPolicy is the inner handler plugged into Deps.WebAIPolicy.
type recordingAIPolicy struct {
	mu    sync.Mutex
	calls []aipolicyCall
}

type aipolicyCall struct {
	method       string
	path         string
	hadPrincipal bool
}

func (r *recordingAIPolicy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := iam.PrincipalFromContext(req.Context())
	r.calls = append(r.calls, aipolicyCall{
		method:       req.Method,
		path:         req.URL.Path,
		hadPrincipal: ok,
	})
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (r *recordingAIPolicy) snapshot() []aipolicyCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]aipolicyCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// newAIPolicyRouter builds a chi router with the AI policy mux
// plugged in. authz=true wires the production RBAC matrix via
// iam.NewRBACAuthorizer; false leaves Authorizer nil so the route
// mounts with RequireAuth only (the path router tests without authz
// have used since SIN-62855).
func newAIPolicyRouter(t *testing.T, role iam.Role, inner http.Handler, authz bool) (http.Handler, *http.Cookie) {
	t.Helper()
	const host = "acme.crm.local"
	acmeID := uuid.New()
	tenants := map[string]uuid.UUID{host: acmeID}
	iamFake := newRoledIAM(tenants)
	iamFake.addUser(host, "alice@acme.test", "pw-alice", role, uuid.New())
	resolver := &fakeResolver{byHost: map[string]*tenancy.Tenant{
		host: {ID: acmeID, Name: "acme", Host: host},
	}}
	deps := httpapi.Deps{
		IAM:            iamFake,
		TenantResolver: resolver,
		WebAIPolicy:    inner,
	}
	if authz {
		deps.Authorizer = iam.NewRBACAuthorizer(iam.RBACConfig{})
	}
	h := httpapi.NewRouter(deps)
	cookie := loginCookie(t, h, host, "alice@acme.test", "pw-alice")
	return h, cookie
}

// TestRouter_WebAIPolicy_GetRequiresSession confirms the page is
// gated on Auth: no cookie → 302 to /login, inner handler never
// runs.
func TestRouter_WebAIPolicy_GetRequiresSession(t *testing.T) {
	t.Parallel()
	inner := &recordingAIPolicy{}
	acmeID := uuid.New()
	resolver := &fakeResolver{byHost: map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}}
	iamFake := newRoledIAM(map[string]uuid.UUID{"acme.crm.local": acmeID})
	h := httpapi.NewRouter(httpapi.Deps{
		IAM:            iamFake,
		TenantResolver: resolver,
		WebAIPolicy:    inner,
	})
	rec := do(t, h, http.MethodGet, "acme.crm.local", "/settings/ai-policy", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("Location = %q, want /login...", loc)
	}
	if got := inner.snapshot(); len(got) != 0 {
		t.Fatalf("inner ran without session: %+v", got)
	}
}

// TestRouter_WebAIPolicy_DeniesNonGerenteRole pins the RBAC gate. A
// session minted as RoleTenantAtendente is denied at RequireAction
// for ActionTenantAIPolicyWrite; the inner handler never sees the
// request.
func TestRouter_WebAIPolicy_DeniesNonGerenteRole(t *testing.T) {
	t.Parallel()
	inner := &recordingAIPolicy{}
	h, cookie := newAIPolicyRouter(t, iam.RoleTenantAtendente, inner, true)
	rec := do(t, h, http.MethodGet, "acme.crm.local", "/settings/ai-policy", nil, cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (atendente cannot manage AI policy); body=%s", rec.Code, rec.Body.String())
	}
	if got := inner.snapshot(); len(got) != 0 {
		t.Fatalf("inner ran for denied role: %+v", got)
	}
}

// TestRouter_WebAIPolicy_AllowsGerenteRole pins the allow path:
// gerente role passes the RBAC gate and the inner handler receives
// the request with iam.Principal in context.
func TestRouter_WebAIPolicy_AllowsGerenteRole(t *testing.T) {
	t.Parallel()
	inner := &recordingAIPolicy{}
	h, cookie := newAIPolicyRouter(t, iam.RoleTenantGerente, inner, true)
	rec := do(t, h, http.MethodGet, "acme.crm.local", "/settings/ai-policy", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := inner.snapshot()
	if len(got) != 1 {
		t.Fatalf("inner call count = %d, want 1 (%+v)", len(got), got)
	}
	if !got[0].hadPrincipal {
		t.Fatalf("inner ran without iam.Principal in context (RequireAuth missing)")
	}
	if got[0].method != http.MethodGet || got[0].path != "/settings/ai-policy" {
		t.Fatalf("inner got %s %s, want GET /settings/ai-policy", got[0].method, got[0].path)
	}
}
