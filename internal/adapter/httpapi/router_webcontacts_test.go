package httpapi_test

// SIN-62855 — WebContacts mount-point integration tests.
//
// The contacts HTMX UI lives in internal/web/contacts; cmd/server
// constructs the inner http.Handler and hands it to httpapi.NewRouter
// via Deps.WebContacts. These tests pin the security envelope chi
// applies on the way in:
//
//   - GET /contacts/{id} requires Auth (302 → /login when no session).
//   - POST /contacts/identity/split passes through CSRF (403 cookie_missing
//     when the __Host-csrf cookie is absent; 200 on the legit path).
//
// They use a recording http.Handler in the WebContacts slot so the
// assertions stay tied to the chi mounting (not the inner template
// rendering, which is covered exhaustively by web/contacts handler tests).

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	csrfmw "github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// postFormWith fires a form-encoded POST with caller-supplied headers
// and cookies, used by the WebContacts integration tests below to drive
// /contacts/identity/split through the chi authed chain.
func postFormWith(t *testing.T, h http.Handler, host, target string, body url.Values, headers map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body.Encode()))
	r.Host = host
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// recordingContacts is the http.Handler we plug into Deps.WebContacts.
// It echoes which method+path reached it so the test can prove the chi
// route table fanned the request through correctly. It also records
// whether the iam.Principal was attached by RequireAuth — that gate is
// what AC #3 of SIN-62855 pins in place ("RequireAuth aplicados").
type recordingContacts struct {
	calls []recordedCall
}

type recordedCall struct {
	method       string
	path         string
	hadPrincipal bool
}

func (r *recordingContacts) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	_, ok := iam.PrincipalFromContext(req.Context())
	r.calls = append(r.calls, recordedCall{
		method:       req.Method,
		path:         req.URL.Path,
		hadPrincipal: ok,
	})
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func newWebContactsRouter(t *testing.T, csrfToken string, contacts http.Handler, recorder *csrfRecorder) (http.Handler, *csrfIAM) {
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
		WebContacts:      contacts,
	})
	return r, iamFake
}

// TestRouter_WebContacts_GetRequiresSession asserts the /contacts/{id}
// route sits behind middleware.Auth. With no session cookie, chi must
// redirect to /login — the recording handler MUST NOT have been called.
func TestRouter_WebContacts_GetRequiresSession(t *testing.T) {
	t.Parallel()
	contacts := &recordingContacts{}
	h, _ := newWebContactsRouter(t, "tok-1", contacts, &csrfRecorder{})
	rec := do(t, h, http.MethodGet, "acme.crm.local", "/contacts/"+uuid.New().String(), nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302 (redirect to /login)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("Location=%q, want /login...", loc)
	}
	if len(contacts.calls) != 0 {
		t.Fatalf("inner handler was called without a session: %+v", contacts.calls)
	}
}

// TestRouter_WebContacts_GetWithSessionReachesHandler proves the GET
// route reaches the inner handler with the iam.Principal already
// attached (RequireAuth ran). This pins the docstring claim on the
// SIN-62855 router branch.
func TestRouter_WebContacts_GetWithSessionReachesHandler(t *testing.T) {
	t.Parallel()
	contacts := &recordingContacts{}
	h, _ := newWebContactsRouter(t, "tok-2", contacts, &csrfRecorder{})
	const host = "acme.crm.local"
	sess, _ := loginAndCookies(t, h, host)

	contactID := uuid.New().String()
	rec := do(t, h, http.MethodGet, host, "/contacts/"+contactID, nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if len(contacts.calls) != 1 {
		t.Fatalf("inner handler call count = %d, want 1 (%+v)", len(contacts.calls), contacts.calls)
	}
	c := contacts.calls[0]
	if c.method != http.MethodGet {
		t.Fatalf("inner method=%q, want GET", c.method)
	}
	if c.path != "/contacts/"+contactID {
		t.Fatalf("inner path=%q, want /contacts/%s", c.path, contactID)
	}
	if !c.hadPrincipal {
		t.Fatalf("inner handler ran without iam.Principal in context (RequireAuth missing)")
	}
}

// TestRouter_WebContacts_PostRejectedWithoutCSRF pins the CSRF gate on
// /contacts/identity/split. Cookie+session present but the __Host-csrf
// cookie is intentionally omitted: the request must 403 with reason
// csrf.cookie_missing and the inner handler MUST NOT run.
func TestRouter_WebContacts_PostRejectedWithoutCSRF(t *testing.T) {
	t.Parallel()
	const csrfToken = "tok-3-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	contacts := &recordingContacts{}
	recorder := &csrfRecorder{}
	h, _ := newWebContactsRouter(t, csrfToken, contacts, recorder)
	const host = "acme.crm.local"

	sess, _ := loginAndCookies(t, h, host)

	body := url.Values{}
	body.Set("link_id", uuid.New().String())
	body.Set("survivor_contact_id", uuid.New().String())
	rec := postFormWith(t, h, host, "/contacts/identity/split", body, map[string]string{
		"Origin": "https://" + host,
	}, sess) // no csrf cookie, no header
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; recorder=%v", rec.Code, recorder.reasons)
	}
	if got := recorder.Last(); got != csrfmw.ReasonCookieMissing {
		t.Fatalf("reason=%q, want %q", got, csrfmw.ReasonCookieMissing)
	}
	if len(contacts.calls) != 0 {
		t.Fatalf("inner handler was called despite CSRF rejection: %+v", contacts.calls)
	}
}

// TestRouter_WebContacts_PostHappyPath covers the green path: valid
// session + valid CSRF cookie/header + same-origin POST → the inner
// handler runs with iam.Principal attached and the recording handler
// observes the original POST path.
func TestRouter_WebContacts_PostHappyPath(t *testing.T) {
	t.Parallel()
	const csrfToken = "tok-4-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	contacts := &recordingContacts{}
	recorder := &csrfRecorder{}
	h, _ := newWebContactsRouter(t, csrfToken, contacts, recorder)
	const host = "acme.crm.local"

	sess, csrfCookie := loginAndCookies(t, h, host)

	body := url.Values{}
	body.Set("link_id", uuid.New().String())
	body.Set("survivor_contact_id", uuid.New().String())
	rec := postFormWith(t, h, host, "/contacts/identity/split", body, map[string]string{
		"Origin":          "https://" + host,
		csrfmw.HeaderName: csrfToken,
	}, sess, csrfCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; recorder=%v", rec.Code, recorder.reasons)
	}
	if len(contacts.calls) != 1 {
		t.Fatalf("inner call count=%d, want 1", len(contacts.calls))
	}
	c := contacts.calls[0]
	if c.method != http.MethodPost || c.path != "/contacts/identity/split" {
		t.Fatalf("inner call=%+v, want POST /contacts/identity/split", c)
	}
	if !c.hadPrincipal {
		t.Fatalf("inner handler ran without iam.Principal in context")
	}
	if len(recorder.reasons) != 0 {
		t.Fatalf("CSRF reasons captured on success path: %v", recorder.reasons)
	}
}

// TestRouter_WebContacts_NilDepsKeepRouteUnmounted proves the
// nil-handler branch in NewRouter: with Deps.WebContacts unset, both
// routes return 404 on the chi router (the chi route table never
// registered them). This is the fail-soft contract documented on the
// Deps.WebContacts field.
func TestRouter_WebContacts_NilDepsKeepRouteUnmounted(t *testing.T) {
	t.Parallel()
	h, _ := newWebContactsRouter(t, "tok-5", nil, &csrfRecorder{})
	const host = "acme.crm.local"
	sess, _ := loginAndCookies(t, h, host)

	rec := do(t, h, http.MethodGet, host, "/contacts/"+uuid.New().String(), nil, sess)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (route not mounted)", rec.Code)
	}
}
