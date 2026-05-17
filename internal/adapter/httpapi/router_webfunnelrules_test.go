package httpapi_test

// SIN-62961 — WebFunnelRules mount-point integration tests.
//
// The funnel-rules HTMX editor lives in internal/web/funnel/rules;
// cmd/server constructs the inner http.Handler and hands it to
// httpapi.NewRouter via Deps.WebFunnelRules. These tests pin the
// security envelope chi applies on the way in:
//
//   - GET /funnel/rules requires Auth (302 → /login when no session).
//   - With a session and Authorizer=nil, the inner handler runs with
//     iam.Principal in context (no per-action gate).
//   - Nil slot keeps every /funnel/rules* path unmounted (404).
//
// The recording http.Handler in the WebFunnelRules slot keeps the
// assertions tied to the chi mounting; the inner template rendering is
// covered by the web/funnel/rules handler tests in their own package.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/tenancy"
)

func newWebFunnelRulesRouter(t *testing.T, csrfToken string, slot http.Handler, recorder *csrfRecorder) (http.Handler, *csrfIAM) {
	t.Helper()
	const host = "acme.crm.local"
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		host: {ID: acmeID, Name: "acme", Host: host},
	}
	tenantIDs := map[string]uuid.UUID{host: acmeID}
	iamFake := newCSRFIAM(tenantIDs, csrfToken)
	iamFake.addUser(host, "alice@acme.test", "pw-alice")
	resolver := &fakeResolver{byHost: tenants}
	r := httpapi.NewRouter(httpapi.Deps{
		IAM:              iamFake,
		TenantResolver:   resolver,
		MasterHost:       "master.crm.local",
		CSRFRejectMetric: recorder.Record,
		WebFunnelRules:   slot,
		// Authorizer intentionally nil — the gate is skipped and the
		// inner handler runs under RequireAuth alone. The handler-level
		// action gate is tested in iam authz tests.
	})
	return r, iamFake
}

func TestRouter_WebFunnelRules_ListRequiresSession(t *testing.T) {
	t.Parallel()
	slot := &recordingContacts{}
	h, _ := newWebFunnelRulesRouter(t, "tok-funnelrules-1", slot, &csrfRecorder{})
	rec := do(t, h, http.MethodGet, "acme.crm.local", "/funnel/rules", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302 (redirect to /login)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("Location=%q, want /login...", loc)
	}
	if len(slot.calls) != 0 {
		t.Fatalf("inner handler was called without a session: %+v", slot.calls)
	}
}

func TestRouter_WebFunnelRules_ListWithSessionReachesInner(t *testing.T) {
	t.Parallel()
	slot := &recordingContacts{}
	h, _ := newWebFunnelRulesRouter(t, "tok-funnelrules-2", slot, &csrfRecorder{})
	const host = "acme.crm.local"
	sess, _ := loginAndCookies(t, h, host)
	rec := do(t, h, http.MethodGet, host, "/funnel/rules", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if len(slot.calls) != 1 {
		t.Fatalf("inner call count=%d, want 1", len(slot.calls))
	}
	c := slot.calls[0]
	if c.method != http.MethodGet || c.path != "/funnel/rules" {
		t.Fatalf("inner call=%+v, want GET /funnel/rules", c)
	}
	if !c.hadPrincipal {
		t.Fatalf("inner handler ran without iam.Principal in context (RequireAuth missing)")
	}
}

func TestRouter_WebFunnelRules_SubroutesAllMount(t *testing.T) {
	t.Parallel()
	slot := &recordingContacts{}
	h, _ := newWebFunnelRulesRouter(t, "tok-funnelrules-3", slot, &csrfRecorder{})
	const host = "acme.crm.local"
	sess, _ := loginAndCookies(t, h, host)
	id := uuid.New().String()
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/funnel/rules/new"},
		{http.MethodGet, "/funnel/rules/trigger-fields"},
		{http.MethodGet, "/funnel/rules/action-fields"},
		{http.MethodGet, "/funnel/rules/preview"},
		{http.MethodGet, "/funnel/rules/" + id + "/edit"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.path, func(t *testing.T) {
			rec := do(t, h, c.method, host, c.path, nil, sess)
			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s %s returned 404 — route not mounted", c.method, c.path)
			}
		})
	}
}

func TestRouter_WebFunnelRules_NilSlotKeepsRoutesUnmounted(t *testing.T) {
	t.Parallel()
	h, _ := newWebFunnelRulesRouter(t, "tok-funnelrules-4", nil, &csrfRecorder{})
	const host = "acme.crm.local"
	sess, _ := loginAndCookies(t, h, host)
	rec := do(t, h, http.MethodGet, host, "/funnel/rules", nil, sess)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (route not mounted)", rec.Code)
	}
}
