package httpapi_test

// SIN-66391 (P2) — WebChannels mount-point integration tests.
//
// The channel-management HTMX UI lives in internal/web/channels; cmd/server
// constructs the inner http.Handler and hands it to httpapi.NewRouter via
// Deps.WebChannels. These tests pin the security envelope chi applies on
// the way in (they use a recording handler so the assertions stay tied to
// the chi mounting, not the inner template rendering — that is covered
// exhaustively by web/channels handler tests):
//
//   - GET /settings/channels requires Auth (302 → /login when no session).
//   - POST /settings/channels/{id}/active passes through CSRF (403
//     cookie_missing when the __Host-csrf cookie is absent; 200 on the
//     legit path) and reaches the inner handler with iam.Principal attached.
//   - Nil Deps.WebChannels keeps every route unmounted (404) — the
//     fail-soft contract documented on the field.
//
// The RequireAction(ActionTenantChannelsManage) gerente gate itself is
// unit-tested in internal/iam (TestRBAC_ChannelsManage) and by the
// RequireAction middleware's own tests; these router tests exercise the
// Authorizer==nil branch (the gate skips, RequireAuth still runs) exactly
// like the sibling WebContacts / WebBranding mount tests.

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

func newWebChannelsRouter(t *testing.T, csrfToken string, channels http.Handler, recorder *csrfRecorder) http.Handler {
	t.Helper()
	const host = "acme.crm.local"
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{host: {ID: acmeID, Name: "acme", Host: host}}
	iamFake := newCSRFIAM(map[string]uuid.UUID{host: acmeID}, csrfToken)
	iamFake.addUser(host, "alice@acme.test", "pw-alice")
	return httpapi.NewRouter(httpapi.Deps{
		IAM:              iamFake,
		TenantResolver:   &fakeResolver{byHost: tenants},
		MasterHost:       "master.crm.local",
		CSRFRejectMetric: recorder.Record,
		WebChannels:      channels,
	})
}

func TestRouter_WebChannels_GetRequiresSession(t *testing.T) {
	t.Parallel()
	rec := &recordingContacts{}
	h := newWebChannelsRouter(t, "tok-c1", rec, &csrfRecorder{})
	got := do(t, h, http.MethodGet, "acme.crm.local", "/settings/channels", nil)
	if got.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302 (redirect to /login)", got.Code)
	}
	if loc := got.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("Location=%q, want /login...", loc)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("inner handler called without a session: %+v", rec.calls)
	}
}

func TestRouter_WebChannels_GetWithSessionReachesHandler(t *testing.T) {
	t.Parallel()
	rec := &recordingContacts{}
	h := newWebChannelsRouter(t, "tok-c2", rec, &csrfRecorder{})
	const host = "acme.crm.local"
	sess, _ := loginAndCookies(t, h, host)

	got := do(t, h, http.MethodGet, host, "/settings/channels", nil, sess)
	if got.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", got.Code)
	}
	if len(rec.calls) != 1 || rec.calls[0].path != "/settings/channels" || !rec.calls[0].hadPrincipal {
		t.Fatalf("inner call wrong: %+v", rec.calls)
	}
}

func TestRouter_WebChannels_TogglePostRejectedWithoutCSRF(t *testing.T) {
	t.Parallel()
	const csrfToken = "tok-c3-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rec := &recordingContacts{}
	recorder := &csrfRecorder{}
	h := newWebChannelsRouter(t, csrfToken, rec, recorder)
	const host = "acme.crm.local"
	sess, _ := loginAndCookies(t, h, host)

	target := "/settings/channels/" + uuid.New().String() + "/active"
	got := postFormWith(t, h, host, target, url.Values{}, map[string]string{
		"Origin": "https://" + host,
	}, sess) // no csrf cookie/header
	if got.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; recorder=%v", got.Code, recorder.reasons)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("inner handler ran despite CSRF rejection: %+v", rec.calls)
	}
}

func TestRouter_WebChannels_TogglePostHappyPath(t *testing.T) {
	t.Parallel()
	const csrfToken = "tok-c4-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rec := &recordingContacts{}
	recorder := &csrfRecorder{}
	h := newWebChannelsRouter(t, csrfToken, rec, recorder)
	const host = "acme.crm.local"
	sess, csrfCookie := loginAndCookies(t, h, host)

	target := "/settings/channels/" + uuid.New().String() + "/active"
	got := postFormWith(t, h, host, target, url.Values{}, map[string]string{
		"Origin":          "https://" + host,
		csrfmw.HeaderName: csrfToken,
	}, sess, csrfCookie)
	if got.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; recorder=%v", got.Code, recorder.reasons)
	}
	if len(rec.calls) != 1 || rec.calls[0].method != http.MethodPost || !rec.calls[0].hadPrincipal {
		t.Fatalf("inner call wrong: %+v", rec.calls)
	}
}

func TestRouter_WebChannels_NilDepsKeepRouteUnmounted(t *testing.T) {
	t.Parallel()
	h := newWebChannelsRouter(t, "tok-c5", nil, &csrfRecorder{})
	const host = "acme.crm.local"
	sess, _ := loginAndCookies(t, h, host)
	got := do(t, h, http.MethodGet, host, "/settings/channels", nil, sess)
	if got.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (route not mounted)", got.Code)
	}
}
