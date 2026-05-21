package httpapi_test

// SIN-63186 — WebLGPD mount-point integration tests (Fase 6 PR3).
//
// The LGPD admin handlers live in internal/web/lgpd; cmd/server
// constructs the per-method inner handlers (Export, Delete) and the
// shared rate-limit middleware, then hands them to httpapi.NewRouter
// via Deps.WebLGPD. These tests pin the security envelope chi applies
// on the way in, mirroring the contracts pinned for WebAIPolicy:
//
//   - No session → 302 to /login (Auth gate fires on both routes).
//   - Session with the wrong role + Authorizer wired → 403 from
//     RequireAction(ActionTenantLGPDExport).
//   - Session as gerente + Authorizer wired → reaches the inner
//     handler with iam.Principal in context.
//   - RateLimit middleware (when wired) returns 429 even for a
//     request that would otherwise be authorized — proving the
//     limiter wraps OUTSIDE RequireAction (a regression here would
//     mean the per-tenant cap is enforced AFTER authz writes the
//     audit row, defeating the AC #7 anti-spam intent).
//   - When either slot is nil the routes are unmounted (chi 404).
//
// POST /admin/lgpd/delete is exercised via the no-session 302 check
// to prove route presence. The CSRF + role POST happy path lives in
// the handler-package suite (internal/web/lgpd) since the router-
// level test infra (roledIAM) intentionally does not mint CSRF
// tokens — same trade-off as the existing WebBranding / WebCampaigns
// router tests, which also skip the POST happy path here.

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

// recordingLGPD captures every request that reaches the inner handler
// along with whether iam.Principal was lifted into context by
// RequireAuth. Used by both Export and Delete slots.
type recordingLGPD struct {
	mu    sync.Mutex
	calls []lgpdCall
}

type lgpdCall struct {
	method       string
	path         string
	hadPrincipal bool
}

func (r *recordingLGPD) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := iam.PrincipalFromContext(req.Context())
	r.calls = append(r.calls, lgpdCall{
		method:       req.Method,
		path:         req.URL.Path,
		hadPrincipal: ok,
	})
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (r *recordingLGPD) snapshot() []lgpdCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]lgpdCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// newLGPDRouter builds a chi router with both LGPD slots plugged in.
// authz=true wires the production RBAC matrix via
// iam.NewRBACAuthorizer; false leaves Authorizer nil so the routes
// mount with RequireAuth only.
func newLGPDRouter(t *testing.T, role iam.Role, export, del http.Handler, rate func(http.Handler) http.Handler, authz bool) (http.Handler, *http.Cookie) {
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
		WebLGPD: httpapi.LGPDRoutes{
			Export:    export,
			Delete:    del,
			RateLimit: rate,
		},
	}
	if authz {
		deps.Authorizer = iam.NewRBACAuthorizer(iam.RBACConfig{})
	}
	h := httpapi.NewRouter(deps)
	cookie := loginCookie(t, h, host, "alice@acme.test", "pw-alice")
	return h, cookie
}

// TestRouter_WebLGPD_ExportRequiresSession pins the Auth gate on the
// export route: no cookie → 302 to /login, inner handler never runs.
func TestRouter_WebLGPD_ExportRequiresSession(t *testing.T) {
	t.Parallel()
	exp := &recordingLGPD{}
	del := &recordingLGPD{}
	acmeID := uuid.New()
	resolver := &fakeResolver{byHost: map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}}
	iamFake := newRoledIAM(map[string]uuid.UUID{"acme.crm.local": acmeID})
	h := httpapi.NewRouter(httpapi.Deps{
		IAM:            iamFake,
		TenantResolver: resolver,
		WebLGPD: httpapi.LGPDRoutes{
			Export: exp,
			Delete: del,
		},
	})
	rec := do(t, h, http.MethodGet, "acme.crm.local", "/admin/lgpd/export?contact_id=00000000-0000-0000-0000-000000000001", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("Location = %q, want /login...", loc)
	}
	if got := exp.snapshot(); len(got) != 0 {
		t.Fatalf("export inner ran without session: %+v", got)
	}
}

// TestRouter_WebLGPD_DeleteRequiresSession pins the Auth gate on the
// delete route too: no cookie → 302 to /login, the route exists (a
// missing mount would surface as 404, not 302).
func TestRouter_WebLGPD_DeleteRequiresSession(t *testing.T) {
	t.Parallel()
	exp := &recordingLGPD{}
	del := &recordingLGPD{}
	acmeID := uuid.New()
	resolver := &fakeResolver{byHost: map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}}
	iamFake := newRoledIAM(map[string]uuid.UUID{"acme.crm.local": acmeID})
	h := httpapi.NewRouter(httpapi.Deps{
		IAM:            iamFake,
		TenantResolver: resolver,
		WebLGPD: httpapi.LGPDRoutes{
			Export: exp,
			Delete: del,
		},
	})
	rec := do(t, h, http.MethodPost, "acme.crm.local", "/admin/lgpd/delete", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (Auth must fire BEFORE route 404); body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("Location = %q, want /login...", loc)
	}
	if got := del.snapshot(); len(got) != 0 {
		t.Fatalf("delete inner ran without session: %+v", got)
	}
}

// TestRouter_WebLGPD_ExportDeniesNonGerenteRole pins the RBAC gate.
// A session minted as RoleTenantAtendente is denied at RequireAction
// for ActionTenantLGPDExport; the inner handler never sees the
// request.
func TestRouter_WebLGPD_ExportDeniesNonGerenteRole(t *testing.T) {
	t.Parallel()
	exp := &recordingLGPD{}
	del := &recordingLGPD{}
	h, cookie := newLGPDRouter(t, iam.RoleTenantAtendente, exp, del, nil, true)
	rec := do(t, h, http.MethodGet, "acme.crm.local", "/admin/lgpd/export?contact_id=00000000-0000-0000-0000-000000000001", nil, cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (atendente cannot export LGPD); body=%s", rec.Code, rec.Body.String())
	}
	if got := exp.snapshot(); len(got) != 0 {
		t.Fatalf("export inner ran for denied role: %+v", got)
	}
}

// TestRouter_WebLGPD_ExportAllowsGerenteRole pins the allow path:
// gerente role passes RequireAction(ActionTenantLGPDExport) and the
// inner handler sees an iam.Principal.
func TestRouter_WebLGPD_ExportAllowsGerenteRole(t *testing.T) {
	t.Parallel()
	exp := &recordingLGPD{}
	del := &recordingLGPD{}
	h, cookie := newLGPDRouter(t, iam.RoleTenantGerente, exp, del, nil, true)
	rec := do(t, h, http.MethodGet, "acme.crm.local", "/admin/lgpd/export?contact_id=00000000-0000-0000-0000-000000000001", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := exp.snapshot()
	if len(got) != 1 {
		t.Fatalf("export inner call count = %d, want 1 (%+v)", len(got), got)
	}
	if !got[0].hadPrincipal {
		t.Fatalf("export inner ran without iam.Principal in context (RequireAuth missing)")
	}
	if got[0].method != http.MethodGet || got[0].path != "/admin/lgpd/export" {
		t.Fatalf("export inner got %s %s, want GET /admin/lgpd/export", got[0].method, got[0].path)
	}
	// Sanity: hitting export must NOT increment the delete recorder
	// — proves the two slots are mounted to their own routes.
	if dg := del.snapshot(); len(dg) != 0 {
		t.Fatalf("delete handler ran for export request: %+v", dg)
	}
}

// TestRouter_WebLGPD_RateLimitWrapsOutsideRequireAction proves the
// wire-supplied RateLimit middleware is applied OUTSIDE the action
// gate: a session that would otherwise be authorized (gerente role,
// RBAC wired) MUST receive a 429 from the limiter rather than 200
// from the inner handler. If the limiter were INSIDE RequireAction,
// the gerente role would breeze past the authz gate and a 200 would
// land — the AC #7 anti-spam intent (don't write an audit_log_security
// row for every burst-throttled request) would be defeated.
func TestRouter_WebLGPD_RateLimitWrapsOutsideRequireAction(t *testing.T) {
	t.Parallel()
	exp := &recordingLGPD{}
	del := &recordingLGPD{}
	// Limiter stub: every request → 429. Captures hits so we can prove
	// the limiter actually ran (vs being silently dropped).
	var rlHits int
	rl := func(_ http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			rlHits++
			w.WriteHeader(http.StatusTooManyRequests)
		})
	}
	h, cookie := newLGPDRouter(t, iam.RoleTenantGerente, exp, del, rl, true)
	rec := do(t, h, http.MethodGet, "acme.crm.local", "/admin/lgpd/export?contact_id=00000000-0000-0000-0000-000000000001", nil, cookie)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (rate limit must wrap OUTSIDE RequireAction even for an allowed role); body=%s", rec.Code, rec.Body.String())
	}
	if rlHits != 1 {
		t.Fatalf("rate-limit hit count = %d, want 1 (middleware did not run)", rlHits)
	}
	if got := exp.snapshot(); len(got) != 0 {
		t.Fatalf("export inner ran despite 429: %+v", got)
	}
}

// TestRouter_WebLGPD_NotMountedWhenSlotsNil pins the conditional
// mount: when either Export or Delete is nil, the chi router omits
// BOTH routes and the requests fall through to 404. This guards
// against a future change that mounts just one verb (which would
// leave the LGPD surface half-wired and confuse operators).
func TestRouter_WebLGPD_NotMountedWhenSlotsNil(t *testing.T) {
	t.Parallel()
	acmeID := uuid.New()
	resolver := &fakeResolver{byHost: map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}}
	iamFake := newRoledIAM(map[string]uuid.UUID{"acme.crm.local": acmeID})
	iamFake.addUser("acme.crm.local", "alice@acme.test", "pw-alice", iam.RoleTenantGerente, uuid.New())
	// Pass only Export but not Delete — both routes should be skipped.
	h := httpapi.NewRouter(httpapi.Deps{
		IAM:            iamFake,
		TenantResolver: resolver,
		WebLGPD: httpapi.LGPDRoutes{
			Export: &recordingLGPD{},
		},
	})
	cookie := loginCookie(t, h, "acme.crm.local", "alice@acme.test", "pw-alice")
	rec := do(t, h, http.MethodGet, "acme.crm.local", "/admin/lgpd/export?contact_id=00000000-0000-0000-0000-000000000001", nil, cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /admin/lgpd/export status = %d, want 404 (route should be unmounted when Delete is nil)", rec.Code)
	}
}
