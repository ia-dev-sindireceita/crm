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
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	csrfmw "github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
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
		"/inbox/conversations/" + uuid.New().String() + "/messages/since",
	} {
		rec := do(t, h, http.MethodGet, host, path, nil, sess)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status=%d for %q, want 404 (route not mounted)", rec.Code, path)
		}
	}
}

// TestRouter_WebInbox_NestedRoutesReachInnerHandler proves the GET
// subtree routes (view, status, and the SIN-65419 messages/since
// live-refresh poll) also reach the inner handler for an authorized
// principal. The chi route table is what we're pinning here — the
// actual handler logic (404 on missing conversation, etc.) is covered
// by web/inbox handler tests. The messages/since case is the standing
// guard against the chi-enumeration miss that recurred on assign
// (SIN-64979), ai-assist (SIN-65004), reset (SIN-65392/65406), and
// again here (SIN-65419): the inner-mux tests pass while the chi mount
// 404s in production.
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
		"/inbox/conversations/" + convID + "/messages/since",
	} {
		rec := do(t, h, http.MethodGet, host, path, nil, sess)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d for %q, want 200 (atendente must reach inbox subtree); body=%q", rec.Code, path, rec.Body.String())
		}
	}
	if len(inboxH.calls) != 3 {
		t.Fatalf("inner call count=%d, want 3 (%+v)", len(inboxH.calls), inboxH.calls)
	}
}

// --- SIN-64979: POST /inbox/conversations/{id}/assign route mount ---
//
// The assign route is a state-changing POST, so it sits behind the
// authed group's RequireCSRF gate *and* RequireAction. roledIAM (above)
// mints role-bearing sessions but no CSRF token, so it cannot drive a
// passing POST. csrfRoledIAM is a self-contained IAMService fake that
// mints sessions carrying both a Role (for RequireAction) and a fixed
// CSRF token (so the login handler sets the __Host-csrf cookie and the
// CSRF triple-match passes). It is independent of roledIAM precisely
// because the SIN-62767 quality bar forbids editing that shared fixture.

const assignCSRFToken = "test-csrf-token-assign-aaaaaaaaaaaaaaaaaaaaaaa"

type csrfRoledIAM struct {
	mu       sync.Mutex
	tenants  map[string]uuid.UUID
	users    map[string]roledUser
	sessions map[uuid.UUID]map[uuid.UUID]roledSession
}

func newCSRFRoledIAM(tenants map[string]uuid.UUID) *csrfRoledIAM {
	return &csrfRoledIAM{
		tenants:  tenants,
		users:    map[string]roledUser{},
		sessions: map[uuid.UUID]map[uuid.UUID]roledSession{},
	}
}

func (s *csrfRoledIAM) addUser(host, email, password string, role iam.Role, userID uuid.UUID) {
	s.users[host+"|"+email] = roledUser{
		tenantID: s.tenants[host],
		userID:   userID,
		password: password,
		role:     role,
	}
}

func (s *csrfRoledIAM) Login(_ context.Context, host, email, password string, _ net.IP, _, _ string) (iam.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tenantID, ok := s.tenants[host]
	if !ok {
		return iam.Session{}, iam.ErrInvalidCredentials
	}
	u, ok := s.users[host+"|"+email]
	if !ok || u.password != password {
		return iam.Session{}, iam.ErrInvalidCredentials
	}
	id := uuid.New()
	sess := roledSession{id: id, userID: u.userID, tenantID: tenantID, role: u.role, expires: time.Now().Add(time.Hour)}
	if s.sessions[tenantID] == nil {
		s.sessions[tenantID] = map[uuid.UUID]roledSession{}
	}
	s.sessions[tenantID][id] = sess
	return iam.Session{ID: id, UserID: u.userID, TenantID: tenantID, Role: u.role, ExpiresAt: sess.expires, CSRFToken: assignCSRFToken}, nil
}

func (s *csrfRoledIAM) Logout(_ context.Context, tenantID, sessionID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.sessions[tenantID]; ok {
		delete(m, sessionID)
	}
	return nil
}

func (s *csrfRoledIAM) ValidateSession(_ context.Context, tenantID, sessionID uuid.UUID) (iam.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.sessions[tenantID]
	if !ok {
		return iam.Session{}, iam.ErrSessionNotFound
	}
	sess, ok := m[sessionID]
	if !ok {
		return iam.Session{}, iam.ErrSessionNotFound
	}
	if time.Now().After(sess.expires) {
		return iam.Session{}, iam.ErrSessionExpired
	}
	return iam.Session{ID: sess.id, UserID: sess.userID, TenantID: sess.tenantID, Role: sess.role, ExpiresAt: sess.expires, CSRFToken: assignCSRFToken}, nil
}

// newAssignRouter wires a router with the recording WebInbox handler and
// a csrfRoledIAM store + production RBAC authorizer, so both the
// RequireAction gate and the RequireCSRF gate are live on the assign POST.
func newAssignRouter(t *testing.T, inboxHandler http.Handler) (http.Handler, *csrfRoledIAM, string) {
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
		WebInbox:       inboxHandler,
		Authorizer: authz.New(authz.Config{
			Inner:    iam.NewRBACAuthorizer(iam.RBACConfig{}),
			Recorder: &authzRecorder{},
			Sampler:  authz.NeverSample{},
		}),
	}
	return httpapi.NewRouter(deps), store, host
}

// loginBothCookies signs the user in and returns the session + CSRF
// cookies the login handler set, so a follow-up POST can satisfy the
// CSRF triple-match.
func loginBothCookies(t *testing.T, h http.Handler, host, email, password string) (sess, csrf *http.Cookie) {
	t.Helper()
	form := url.Values{}
	form.Set("email", email)
	form.Set("password", password)
	rec := do(t, h, http.MethodPost, host, "/login", strings.NewReader(form.Encode()))
	if rec.Code != http.StatusFound {
		t.Fatalf("login status=%d, want 302; body=%q", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case sessioncookie.NameTenant:
			sess = c
		case sessioncookie.NameCSRF:
			csrf = c
		}
	}
	if sess == nil || csrf == nil {
		t.Fatalf("login did not set both cookies: sess=%v csrf=%v", sess, csrf)
	}
	return sess, csrf
}

// postAssign fires POST /inbox/conversations/{id}/assign with a fully
// valid CSRF presentation (cookie + matching header + same-origin),
// isolating the route-table / RequireAction behaviour from CSRF noise.
func postAssign(t *testing.T, h http.Handler, host, convID, targetUserID string, sess, csrf *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	form.Set("targetUserID", targetUserID)
	r := httptest.NewRequest(http.MethodPost, "/inbox/conversations/"+convID+"/assign", strings.NewReader(form.Encode()))
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

// TestRouter_WebInbox_AssignRouteReachableForAtendente is the Blocker-1
// regression (SecurityEngineer + CTO review of PR #317): the POST assign
// route was registered only on the inner mux, so chi 404'd it before the
// handler. This pins the chi route table — an authorized atendente, with
// a valid CSRF presentation, reaches the inner handler with POST on the
// assign path. Without the router.go mount this fails with 404.
func TestRouter_WebInbox_AssignRouteReachableForAtendente(t *testing.T) {
	t.Parallel()
	inboxH := &recordingInbox{}
	h, store, host := newAssignRouter(t, inboxH)
	store.addUser(host, "atendente@acme.test", "pw", iam.RoleTenantAtendente, uuid.New())

	sess, csrf := loginBothCookies(t, h, host, "atendente@acme.test", "pw")
	convID := uuid.New().String()
	rec := postAssign(t, h, host, convID, uuid.New().String(), sess, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (atendente must reach POST assign); body=%q", rec.Code, rec.Body.String())
	}
	if len(inboxH.calls) != 1 {
		t.Fatalf("inner call count=%d, want 1 (%+v)", len(inboxH.calls), inboxH.calls)
	}
	c := inboxH.calls[0]
	if c.method != http.MethodPost || c.path != "/inbox/conversations/"+convID+"/assign" {
		t.Fatalf("inner call=%+v, want POST .../assign", c)
	}
	if !c.hadPrincipal {
		t.Fatalf("inner handler ran without iam.Principal (RequireAuth missing)")
	}
}

// TestRouter_WebInbox_AssignRouteDeniedForCommon proves the assign POST
// inherits the RequireAction(ActionTenantInboxRead) gate: a Common-role
// session — with a fully valid CSRF presentation, so the 403 can only
// come from RequireAction, not CSRF — is denied before the inner handler
// runs.
func TestRouter_WebInbox_AssignRouteDeniedForCommon(t *testing.T) {
	t.Parallel()
	inboxH := &recordingInbox{}
	h, store, host := newAssignRouter(t, inboxH)
	store.addUser(host, "common@acme.test", "pw", iam.RoleTenantCommon, uuid.New())

	sess, csrf := loginBothCookies(t, h, host, "common@acme.test", "pw")
	rec := postAssign(t, h, host, uuid.New().String(), uuid.New().String(), sess, csrf)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (common denied at RequireAction, CSRF valid); body=%q", rec.Code, rec.Body.String())
	}
	if len(inboxH.calls) != 0 {
		t.Fatalf("inner handler ran on a deny path: %+v", inboxH.calls)
	}
}

// --- SIN-65004: POST /inbox/conversations/{id}/ai-assist route mount ---
//
// Same defect class as the assign route above (SIN-64979): the ai-assist
// POST was registered only on the inner mux (web/inbox Routes, conditional
// on AIAssist.Summarizer), so chi 404'd it before the handler in
// production. The recording WebInbox handler used here stands in for the
// inner mux, so these tests pin the chi route table + security envelope
// (RequireAuth → RequireAction → RequireCSRF), not the feature gating.

// postAIAssist fires POST /inbox/conversations/{id}/ai-assist with a fully
// valid CSRF presentation (cookie + matching header + same-origin), so the
// route-table / RequireAction behaviour is isolated from CSRF noise. It
// reuses the assignCSRFToken/csrfRoledIAM seam from the assign tests.
func postAIAssist(t *testing.T, h http.Handler, host, convID string, sess, csrf *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/inbox/conversations/"+convID+"/ai-assist", nil)
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

// TestRouter_WebInbox_AIAssistRouteReachableForAtendente pins the chi
// route table for the ai-assist POST: an authorized atendente, with a
// valid CSRF presentation, reaches the inner handler with POST on the
// ai-assist path. Without the router.go mount this fails with 404.
func TestRouter_WebInbox_AIAssistRouteReachableForAtendente(t *testing.T) {
	t.Parallel()
	inboxH := &recordingInbox{}
	h, store, host := newAssignRouter(t, inboxH)
	store.addUser(host, "atendente@acme.test", "pw", iam.RoleTenantAtendente, uuid.New())

	sess, csrf := loginBothCookies(t, h, host, "atendente@acme.test", "pw")
	convID := uuid.New().String()
	rec := postAIAssist(t, h, host, convID, sess, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (atendente must reach POST ai-assist); body=%q", rec.Code, rec.Body.String())
	}
	if len(inboxH.calls) != 1 {
		t.Fatalf("inner call count=%d, want 1 (%+v)", len(inboxH.calls), inboxH.calls)
	}
	c := inboxH.calls[0]
	if c.method != http.MethodPost || c.path != "/inbox/conversations/"+convID+"/ai-assist" {
		t.Fatalf("inner call=%+v, want POST .../ai-assist", c)
	}
	if !c.hadPrincipal {
		t.Fatalf("inner handler ran without iam.Principal (RequireAuth missing)")
	}
}

// TestRouter_WebInbox_AIAssistRouteDeniedForCommon proves the ai-assist
// POST inherits the RequireAction(ActionTenantInboxRead) gate: a
// Common-role session — with a fully valid CSRF presentation, so the 403
// can only come from RequireAction, not CSRF — is denied before the inner
// handler runs.
func TestRouter_WebInbox_AIAssistRouteDeniedForCommon(t *testing.T) {
	t.Parallel()
	inboxH := &recordingInbox{}
	h, store, host := newAssignRouter(t, inboxH)
	store.addUser(host, "common@acme.test", "pw", iam.RoleTenantCommon, uuid.New())

	sess, csrf := loginBothCookies(t, h, host, "common@acme.test", "pw")
	rec := postAIAssist(t, h, host, uuid.New().String(), sess, csrf)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (common denied at RequireAction, CSRF valid); body=%q", rec.Code, rec.Body.String())
	}
	if len(inboxH.calls) != 0 {
		t.Fatalf("inner handler ran on a deny path: %+v", inboxH.calls)
	}
}

// --- SIN-65392 / SIN-65406: POST /inbox/conversations/{id}/reset route mount ---
//
// Same defect class as the assign (SIN-64979) and ai-assist (SIN-65004)
// routes above: PR #391 registered the reset POST only on the inner mux
// (web/inbox Routes, conditional on the ResetConversation dep) plus the
// "Apagar mensagens" button and the use-case wiring, but omitted the chi
// mount in router.go — so chi 404'd the POST before the handler while the
// button still rendered (confirmed on staging, SIN-65406). The inner-mux
// handler tests passed because they bypass chi. These tests pin the chi
// route table + security envelope (RequireAuth → RequireAction → RequireCSRF).

// postReset fires POST /inbox/conversations/{id}/reset with a fully valid
// CSRF presentation, mirroring postAIAssist so the route-table / RequireAction
// behaviour is isolated from CSRF noise.
func postReset(t *testing.T, h http.Handler, host, convID string, sess, csrf *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/inbox/conversations/"+convID+"/reset", nil)
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

// TestRouter_WebInbox_ResetRouteReachableForAtendente pins the chi route
// table for the reset POST: an authorized atendente, with a valid CSRF
// presentation, reaches the inner handler with POST on the reset path.
// Without the router.go mount this fails with 404 (the regression this
// test guards — SIN-65406).
func TestRouter_WebInbox_ResetRouteReachableForAtendente(t *testing.T) {
	t.Parallel()
	inboxH := &recordingInbox{}
	h, store, host := newAssignRouter(t, inboxH)
	store.addUser(host, "atendente@acme.test", "pw", iam.RoleTenantAtendente, uuid.New())

	sess, csrf := loginBothCookies(t, h, host, "atendente@acme.test", "pw")
	convID := uuid.New().String()
	rec := postReset(t, h, host, convID, sess, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (atendente must reach POST reset); body=%q", rec.Code, rec.Body.String())
	}
	if len(inboxH.calls) != 1 {
		t.Fatalf("inner call count=%d, want 1 (%+v)", len(inboxH.calls), inboxH.calls)
	}
	c := inboxH.calls[0]
	if c.method != http.MethodPost || c.path != "/inbox/conversations/"+convID+"/reset" {
		t.Fatalf("inner call=%+v, want POST .../reset", c)
	}
	if !c.hadPrincipal {
		t.Fatalf("inner handler ran without iam.Principal (RequireAuth missing)")
	}
}

// TestRouter_WebInbox_ResetRouteDeniedForCommon proves the reset POST
// inherits the RequireAction(ActionTenantInboxRead) gate: a Common-role
// session — with a fully valid CSRF presentation, so the 403 can only come
// from RequireAction, not CSRF — is denied before the inner handler runs.
func TestRouter_WebInbox_ResetRouteDeniedForCommon(t *testing.T) {
	t.Parallel()
	inboxH := &recordingInbox{}
	h, store, host := newAssignRouter(t, inboxH)
	store.addUser(host, "common@acme.test", "pw", iam.RoleTenantCommon, uuid.New())

	sess, csrf := loginBothCookies(t, h, host, "common@acme.test", "pw")
	rec := postReset(t, h, host, uuid.New().String(), sess, csrf)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (common denied at RequireAction, CSRF valid); body=%q", rec.Code, rec.Body.String())
	}
	if len(inboxH.calls) != 0 {
		t.Fatalf("inner handler ran on a deny path: %+v", inboxH.calls)
	}
}

// avoid unused import error when middleware.SessionFromContext changes
// upstream; keep the import alive so future test growth has the
// session helpers at hand.
var _ = middleware.SessionFromContext
