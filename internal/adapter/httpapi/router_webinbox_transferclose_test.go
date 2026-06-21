package httpapi_test

// SIN-65473 — POST /inbox/conversations/{id}/transfer, .../close, .../reopen
// route mounts.
//
// Same defect class as the assign (SIN-64979), ai-assist (SIN-65004), reset
// (SIN-65406), and live-poll (SIN-65419) routes: the inner mux (web/inbox
// Routes) registers these conditionally, but chi enumerates the /inbox
// subtree route-by-route, so each POST must be mounted in router.go or it
// 404s before the handler while the rendered button posts into the void.
// These tests pin the chi route table + security envelope (RequireAuth →
// RequireAction(ActionTenantInboxRead) → RequireCSRF) using the recording
// WebInbox handler + the csrfRoledIAM seam from router_webinbox_test.go.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	csrfmw "github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/iam"
)

// postInboxAction fires a state-changing POST against one of the new inbox
// write routes with a fully valid CSRF presentation (cookie + matching
// header + same-origin), so the route-table / RequireAction behaviour is
// isolated from CSRF noise. form may be nil (close/reopen carry no body).
func postInboxAction(t *testing.T, h http.Handler, host, path string, form url.Values, sess, csrf *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(http.MethodPost, path, body)
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

// TestRouter_WebInbox_TransferCloseReopenReachableForAtendente pins the chi
// route table for all three new write routes: an authorized atendente, with
// a valid CSRF presentation, reaches the inner handler with POST on each
// path. Without the router.go mounts these fail with 404.
func TestRouter_WebInbox_TransferCloseReopenReachableForAtendente(t *testing.T) {
	t.Parallel()
	inboxH := &recordingInbox{}
	h, store, host := newAssignRouter(t, inboxH)
	store.addUser(host, "atendente@acme.test", "pw", iam.RoleTenantAtendente, uuid.New())

	sess, csrf := loginBothCookies(t, h, host, "atendente@acme.test", "pw")
	convID := uuid.New().String()

	cases := []struct {
		name string
		path string
		form url.Values
	}{
		{"transfer", "/inbox/conversations/" + convID + "/transfer", url.Values{"targetUserID": {uuid.New().String()}}},
		{"close", "/inbox/conversations/" + convID + "/close", nil},
		{"reopen", "/inbox/conversations/" + convID + "/reopen", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postInboxAction(t, h, host, tc.path, tc.form, sess, csrf)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200 (atendente must reach POST %s); body=%q", rec.Code, tc.name, rec.Body.String())
			}
		})
	}
	if len(inboxH.calls) != len(cases) {
		t.Fatalf("inner call count=%d, want %d (%+v)", len(inboxH.calls), len(cases), inboxH.calls)
	}
	for i, c := range inboxH.calls {
		if c.method != http.MethodPost || c.path != cases[i].path {
			t.Fatalf("inner call[%d]=%+v, want POST %s", i, c, cases[i].path)
		}
		if !c.hadPrincipal {
			t.Fatalf("inner handler ran without iam.Principal on %s (RequireAuth missing)", cases[i].name)
		}
	}
}

// TestRouter_WebInbox_TransferCloseReopenDeniedForCommon proves the three
// new POSTs inherit the RequireAction(ActionTenantInboxRead) gate: a
// Common-role session — with a fully valid CSRF presentation, so the 403 can
// only come from RequireAction, not CSRF — is denied before the inner
// handler runs.
func TestRouter_WebInbox_TransferCloseReopenDeniedForCommon(t *testing.T) {
	t.Parallel()
	inboxH := &recordingInbox{}
	h, store, host := newAssignRouter(t, inboxH)
	store.addUser(host, "common@acme.test", "pw", iam.RoleTenantCommon, uuid.New())

	sess, csrf := loginBothCookies(t, h, host, "common@acme.test", "pw")
	convID := uuid.New().String()

	for _, path := range []string{
		"/inbox/conversations/" + convID + "/transfer",
		"/inbox/conversations/" + convID + "/close",
		"/inbox/conversations/" + convID + "/reopen",
	} {
		rec := postInboxAction(t, h, host, path, nil, sess, csrf)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status=%d for %s, want 403 (common denied at RequireAction, CSRF valid); body=%q", rec.Code, path, rec.Body.String())
		}
	}
	if len(inboxH.calls) != 0 {
		t.Fatalf("inner handler ran on a deny path: %+v", inboxH.calls)
	}
}
