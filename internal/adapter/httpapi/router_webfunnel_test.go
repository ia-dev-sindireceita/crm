package httpapi_test

// SIN-62862 — WebFunnel mount-point integration tests.
//
// The funnel HTMX UI lives in internal/web/funnel; cmd/server constructs
// the inner http.Handler and hands it to httpapi.NewRouter via
// Deps.WebFunnel. These tests pin the security envelope chi applies on
// the way in, mirroring the contracts tested for SIN-62855's WebContacts:
//
//   - GET /funnel requires Auth (302 → /login when no session).
//   - POST /funnel/transitions passes through CSRF (403 cookie_missing
//     when the __Host-csrf cookie is absent; 200 on the legit path).
//   - The history modal + close routes also hit the inner handler with
//     an iam.Principal in context.
//
// The recording http.Handler in the WebFunnel slot keeps the assertions
// tied to the chi mounting; the inner template rendering is covered by
// the web/funnel handler tests in their own package.

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	csrfmw "github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// newWebFunnelRouter wires a chi.Router whose WebFunnel slot is the
// caller-supplied http.Handler. Mirror of newWebContactsRouter so the
// fork stays auditable.
func newWebFunnelRouter(t *testing.T, csrfToken string, funnel http.Handler, recorder *csrfRecorder) (http.Handler, *csrfIAM) {
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
		WebFunnel:        funnel,
	})
	return r, iamFake
}

func TestRouter_WebFunnel_BoardRequiresSession(t *testing.T) {
	t.Parallel()
	funnel := &recordingContacts{}
	h, _ := newWebFunnelRouter(t, "tok-funnel-1", funnel, &csrfRecorder{})
	rec := do(t, h, http.MethodGet, "acme.crm.local", "/funnel", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302 (redirect to /login)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("Location=%q, want /login...", loc)
	}
	if len(funnel.calls) != 0 {
		t.Fatalf("inner handler was called without a session: %+v", funnel.calls)
	}
}

func TestRouter_WebFunnel_BoardWithSessionReachesHandler(t *testing.T) {
	t.Parallel()
	funnel := &recordingContacts{}
	h, _ := newWebFunnelRouter(t, "tok-funnel-2", funnel, &csrfRecorder{})
	const host = "acme.crm.local"
	sess, _ := loginAndCookies(t, h, host)

	rec := do(t, h, http.MethodGet, host, "/funnel", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if len(funnel.calls) != 1 {
		t.Fatalf("inner call count=%d, want 1 (%+v)", len(funnel.calls), funnel.calls)
	}
	c := funnel.calls[0]
	if c.method != http.MethodGet {
		t.Fatalf("inner method=%q, want GET", c.method)
	}
	if c.path != "/funnel" {
		t.Fatalf("inner path=%q, want /funnel", c.path)
	}
	if !c.hadPrincipal {
		t.Fatalf("inner handler ran without iam.Principal in context (RequireAuth missing)")
	}
}

func TestRouter_WebFunnel_HistoryAndModalReachInnerHandler(t *testing.T) {
	t.Parallel()
	funnel := &recordingContacts{}
	h, _ := newWebFunnelRouter(t, "tok-funnel-3", funnel, &csrfRecorder{})
	const host = "acme.crm.local"
	sess, _ := loginAndCookies(t, h, host)

	convID := uuid.New().String()
	historyPath := "/funnel/conversations/" + convID + "/history"
	cases := []struct {
		name string
		path string
	}{
		{"history modal", historyPath},
		{"modal close", "/funnel/modal/close"},
	}
	for i, c := range cases {
		i, c := i, c
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, host, c.path, nil, sess)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s status=%d, want 200", c.path, rec.Code)
			}
			if len(funnel.calls) <= i {
				t.Fatalf("inner handler not called for %s", c.path)
			}
			call := funnel.calls[i]
			if call.path != c.path {
				t.Fatalf("inner path=%q, want %q", call.path, c.path)
			}
			if !call.hadPrincipal {
				t.Fatalf("inner handler ran without iam.Principal in context")
			}
		})
	}
}

func TestRouter_WebFunnel_TransitionsRejectedWithoutCSRF(t *testing.T) {
	t.Parallel()
	const csrfToken = "tok-funnel-4-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	funnel := &recordingContacts{}
	recorder := &csrfRecorder{}
	h, _ := newWebFunnelRouter(t, csrfToken, funnel, recorder)
	const host = "acme.crm.local"

	sess, _ := loginAndCookies(t, h, host)

	body := url.Values{}
	body.Set("conversation_id", uuid.New().String())
	body.Set("to_stage_key", "qualificando")
	rec := postFormWith(t, h, host, "/funnel/transitions", body, map[string]string{
		"Origin": "https://" + host,
	}, sess) // no csrf cookie, no header
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; recorder=%v", rec.Code, recorder.reasons)
	}
	if got := recorder.Last(); got != csrfmw.ReasonCookieMissing {
		t.Fatalf("reason=%q, want %q", got, csrfmw.ReasonCookieMissing)
	}
	if len(funnel.calls) != 0 {
		t.Fatalf("inner handler was called despite CSRF rejection: %+v", funnel.calls)
	}
}

func TestRouter_WebFunnel_TransitionsHappyPath(t *testing.T) {
	t.Parallel()
	const csrfToken = "tok-funnel-5-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	funnel := &recordingContacts{}
	recorder := &csrfRecorder{}
	h, _ := newWebFunnelRouter(t, csrfToken, funnel, recorder)
	const host = "acme.crm.local"

	sess, csrfCookie := loginAndCookies(t, h, host)

	body := url.Values{}
	body.Set("conversation_id", uuid.New().String())
	body.Set("to_stage_key", "qualificando")
	rec := postFormWith(t, h, host, "/funnel/transitions", body, map[string]string{
		"Origin":          "https://" + host,
		csrfmw.HeaderName: csrfToken,
	}, sess, csrfCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; recorder=%v", rec.Code, recorder.reasons)
	}
	if len(funnel.calls) != 1 {
		t.Fatalf("inner call count=%d, want 1", len(funnel.calls))
	}
	c := funnel.calls[0]
	if c.method != http.MethodPost || c.path != "/funnel/transitions" {
		t.Fatalf("inner call=%+v, want POST /funnel/transitions", c)
	}
	if !c.hadPrincipal {
		t.Fatalf("inner handler ran without iam.Principal in context")
	}
	if len(recorder.reasons) != 0 {
		t.Fatalf("CSRF reasons captured on success path: %v", recorder.reasons)
	}
}

func TestRouter_WebFunnel_NilDepsKeepRoutesUnmounted(t *testing.T) {
	t.Parallel()
	h, _ := newWebFunnelRouter(t, "tok-funnel-6", nil, &csrfRecorder{})
	const host = "acme.crm.local"
	sess, _ := loginAndCookies(t, h, host)

	rec := do(t, h, http.MethodGet, host, "/funnel", nil, sess)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (route not mounted)", rec.Code)
	}
}
