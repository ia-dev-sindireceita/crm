package httpapi_test

// SIN-65364 — WebAIPanel mount-point integration tests.
//
// The inbox AI-assist consent modal POSTs to /aipanel/consent/accept
// and /cancel. cmd/server's aipanel_wire.go builds the inner
// http.Handler (an *http.ServeMux from aipanel.Handler.Routes) and
// hands it to httpapi.NewRouter via Deps.WebAIPanel. Before this mount
// the routes 404'd and confirming the modal left the operator stuck.
//
// These tests pin the chi security envelope the way the SIN-65004
// ai-assist tests pin theirs: the cancel POST (the cheapest route that
// needs no DB) reaches the inner handler for an atendente with a valid
// CSRF presentation, is denied for Common at the RequireAction gate,
// and 404s when the dep is nil. They reuse the recordingInbox handler
// and csrfRoledIAM / loginBothCookies helpers from
// router_webinbox_test.go (same package).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	csrfmw "github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/authz"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// newAIPanelRouter wires a router with the supplied WebAIPanel slot, a
// CSRF-capable IAM store, and the production RBAC authorizer so both
// the RequireAction gate and the RequireCSRF gate are live on the POST.
func newAIPanelRouter(t *testing.T, panelHandler http.Handler) (http.Handler, *csrfRoledIAM, string) {
	t.Helper()
	const host = "acme.crm.local"
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{host: {ID: acmeID, Name: "acme", Host: host}}
	tenantIDs := map[string]uuid.UUID{host: acmeID}
	store := newCSRFRoledIAM(tenantIDs)
	resolver := &fakeResolver{byHost: tenants}
	deps := httpapi.Deps{
		IAM:            store,
		TenantResolver: resolver,
		WebAIPanel:     panelHandler,
		Authorizer: authz.New(authz.Config{
			Inner:    iam.NewRBACAuthorizer(iam.RBACConfig{}),
			Recorder: &authzRecorder{},
			Sampler:  authz.NeverSample{},
		}),
	}
	return httpapi.NewRouter(deps), store, host
}

// postCancel fires POST /aipanel/consent/cancel with a fully valid CSRF
// presentation (cookie + matching header + same-origin) so the only
// gate that can reject is RequireAction.
func postCancel(t *testing.T, h http.Handler, host string, sess, csrf *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/aipanel/consent/cancel", strings.NewReader("scope_kind=channel"))
	r.Host = host
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://"+host)
	r.Header.Set(csrfmw.HeaderName, assignCSRFToken)
	r.AddCookie(sess)
	r.AddCookie(csrf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestRouter_WebAIPanel_CancelReachableForAtendente is the SIN-65364
// Bug 1 regression: the consent routes were registered only on the
// inner mux, so chi 404'd them before the handler. An authorized
// atendente with a valid CSRF presentation must reach the inner handler
// with POST on the cancel path. Without the router.go mount this 404s.
func TestRouter_WebAIPanel_CancelReachableForAtendente(t *testing.T) {
	t.Parallel()
	panelH := &recordingInbox{}
	h, store, host := newAIPanelRouter(t, panelH)
	store.addUser(host, "atendente@acme.test", "pw", iam.RoleTenantAtendente, uuid.New())

	sess, csrf := loginBothCookies(t, h, host, "atendente@acme.test", "pw")
	rec := postCancel(t, h, host, sess, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (atendente must reach POST cancel); body=%q", rec.Code, rec.Body.String())
	}
	if len(panelH.calls) != 1 {
		t.Fatalf("inner call count=%d, want 1 (%+v)", len(panelH.calls), panelH.calls)
	}
	c := panelH.calls[0]
	if c.method != http.MethodPost || c.path != "/aipanel/consent/cancel" {
		t.Fatalf("inner call=%+v, want POST /aipanel/consent/cancel", c)
	}
	if !c.hadPrincipal {
		t.Fatalf("inner handler ran without iam.Principal (RequireAuth missing)")
	}
}

// TestRouter_WebAIPanel_CancelDeniedForCommon proves the consent POST
// inherits RequireAction(ActionTenantInboxRead): a Common-role session
// (valid CSRF, so the 403 can only come from RequireAction) is denied
// before the inner handler runs.
func TestRouter_WebAIPanel_CancelDeniedForCommon(t *testing.T) {
	t.Parallel()
	panelH := &recordingInbox{}
	h, store, host := newAIPanelRouter(t, panelH)
	store.addUser(host, "common@acme.test", "pw", iam.RoleTenantCommon, uuid.New())

	sess, csrf := loginBothCookies(t, h, host, "common@acme.test", "pw")
	rec := postCancel(t, h, host, sess, csrf)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (common denied at RequireAction, CSRF valid); body=%q", rec.Code, rec.Body.String())
	}
	if len(panelH.calls) != 0 {
		t.Fatalf("inner handler ran on a deny path: %+v", panelH.calls)
	}
}

// TestRouter_WebAIPanel_NilDepUnmounted asserts the fail-soft default:
// when Deps.WebAIPanel is nil the consent routes stay unmounted and chi
// returns 404 (the inner handler is never constructed).
func TestRouter_WebAIPanel_NilDepUnmounted(t *testing.T) {
	t.Parallel()
	h, store, host := newAIPanelRouter(t, nil)
	store.addUser(host, "atendente@acme.test", "pw", iam.RoleTenantAtendente, uuid.New())

	sess, csrf := loginBothCookies(t, h, host, "atendente@acme.test", "pw")
	rec := postCancel(t, h, host, sess, csrf)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (nil WebAIPanel must stay unmounted); body=%q", rec.Code, rec.Body.String())
	}
}
