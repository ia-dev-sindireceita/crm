package httpapi_test

import (
	"context"
	"errors"
	"io"
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
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// fakeResolver maps host -> tenant. nil mapping returns ErrTenantNotFound.
type fakeResolver struct {
	byHost map[string]*tenancy.Tenant
}

func (f *fakeResolver) ResolveByHost(_ context.Context, host string) (*tenancy.Tenant, error) {
	if t, ok := f.byHost[host]; ok {
		return t, nil
	}
	return nil, tenancy.ErrTenantNotFound
}

// inmemIAM is the bare iam.Service shape the router needs, with the
// Login/ValidateSession/Logout semantics of the real Service but driven by
// in-memory maps. It mirrors the per-tenant scoping property: a session
// created for tenant A is invisible to tenant B (Acceptance Criterion #2
// of SIN-62217 / Fase 0).
type inmemIAM struct {
	mu       sync.Mutex
	users    map[string]userRow                  // key: host|email
	sessions map[uuid.UUID]map[uuid.UUID]session // tenantID -> sessionID -> session
	tenants  map[string]uuid.UUID                // host -> tenantID
}

type userRow struct {
	tenantID uuid.UUID
	userID   uuid.UUID
	password string
}

type session struct {
	id       uuid.UUID
	userID   uuid.UUID
	tenantID uuid.UUID
	expires  time.Time
}

func newInmemIAM(tenants map[string]uuid.UUID) *inmemIAM {
	return &inmemIAM{
		users:    map[string]userRow{},
		sessions: map[uuid.UUID]map[uuid.UUID]session{},
		tenants:  tenants,
	}
}

func (s *inmemIAM) addUser(host, email, password string, userID uuid.UUID) {
	tenantID := s.tenants[host]
	s.users[host+"|"+email] = userRow{tenantID: tenantID, userID: userID, password: password}
}

func (s *inmemIAM) Login(_ context.Context, host, email, password string, _ net.IP, _, _ string) (iam.Session, error) {
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
	sess := session{id: id, userID: u.userID, tenantID: tenantID, expires: time.Now().Add(time.Hour)}
	if s.sessions[tenantID] == nil {
		s.sessions[tenantID] = map[uuid.UUID]session{}
	}
	s.sessions[tenantID][id] = sess
	return iam.Session{ID: id, UserID: u.userID, TenantID: tenantID, ExpiresAt: sess.expires}, nil
}

func (s *inmemIAM) Logout(_ context.Context, tenantID, sessionID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.sessions[tenantID]; ok {
		delete(m, sessionID)
	}
	return nil
}

func (s *inmemIAM) ValidateSession(_ context.Context, tenantID, sessionID uuid.UUID) (iam.Session, error) {
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
	return iam.Session{ID: sess.id, UserID: sess.userID, TenantID: sess.tenantID, ExpiresAt: sess.expires}, nil
}

func newRouter(t *testing.T) (http.Handler, *inmemIAM, map[string]*tenancy.Tenant) {
	t.Helper()
	acmeID := uuid.New()
	globexID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local":   {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
		"globex.crm.local": {ID: globexID, Name: "globex", Host: "globex.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{
		"acme.crm.local":   acmeID,
		"globex.crm.local": globexID,
	}
	store := newInmemIAM(tenantIDs)
	store.addUser("acme.crm.local", "alice@acme.test", "pw-alice", uuid.New())
	store.addUser("globex.crm.local", "bob@globex.test", "pw-bob", uuid.New())

	resolver := &fakeResolver{byHost: tenants}
	r := httpapi.NewRouter(httpapi.Deps{
		IAM:            store,
		TenantResolver: resolver,
	})
	return r, store, tenants
}

func do(t *testing.T, h http.Handler, method, host, target string, body io.Reader, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, body)
	r.Host = host
	if body != nil && method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestRouter_Health_BypassesTenantScope(t *testing.T) {
	t.Parallel()
	h, _, _ := newRouter(t)

	rec := do(t, h, http.MethodGet, "totally-unknown-host.example", "/health", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (health must not require tenant)", rec.Code)
	}
}

func TestRouter_HelloTenant_RedirectsWhenNoCookie(t *testing.T) {
	t.Parallel()
	h, _, _ := newRouter(t)

	rec := do(t, h, http.MethodGet, "acme.crm.local", "/hello-tenant", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302 (auth missing -> redirect)", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login?next=") {
		t.Fatalf("Location=%q does not start with /login?next=", loc)
	}
}

func TestRouter_UnknownHost_404Generic(t *testing.T) {
	t.Parallel()
	h, _, _ := newRouter(t)
	rec := do(t, h, http.MethodGet, "evil.crm.local", "/hello-tenant", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (TenantScope generic body)", rec.Code)
	}
}

func TestRouter_LoginGet_RendersFormPerTenant(t *testing.T) {
	t.Parallel()
	h, _, _ := newRouter(t)
	rec := do(t, h, http.MethodGet, "acme.crm.local", "/login", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<form") {
		t.Fatalf("body missing form: %q", rec.Body.String())
	}
}

func TestRouter_LoginPost_ThenHelloTenant_BodyContainsTenantName(t *testing.T) {
	t.Parallel()
	h, _, _ := newRouter(t)

	form := url.Values{}
	form.Set("email", "alice@acme.test")
	form.Set("password", "pw-alice")
	form.Set("next", "/hello-tenant")

	loginRec := do(t, h, http.MethodPost, "acme.crm.local", "/login", strings.NewReader(form.Encode()))
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status=%d, want 302", loginRec.Code)
	}
	cookies := loginRec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != middleware.SessionCookieName {
		t.Fatalf("login did not set session cookie: %+v", cookies)
	}
	sessionCookie := cookies[0]
	if !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode || sessionCookie.Path != "/" {
		t.Fatalf("cookie attrs wrong: %+v", sessionCookie)
	}

	helloRec := do(t, h, http.MethodGet, "acme.crm.local", "/hello-tenant", nil, sessionCookie)
	if helloRec.Code != http.StatusOK {
		t.Fatalf("hello status=%d, want 200", helloRec.Code)
	}
	if !strings.Contains(helloRec.Body.String(), "acme") {
		t.Fatalf("body does not contain tenant name: %q", helloRec.Body.String())
	}
}

// TestRouter_HelloTenant_IsolationBetweenTenants is Acceptance Criterion #2
// of SIN-62217 / Fase 0: a session minted for tenant `acme` must NOT
// authenticate on `globex.crm.local`. The session-store is per-tenant, so
// ValidateSession returns ErrSessionNotFound when probed under the wrong
// tenant id, and Auth collapses that into the same /login redirect any
// missing-cookie request gets.
func TestRouter_HelloTenant_IsolationBetweenTenants(t *testing.T) {
	t.Parallel()
	h, _, _ := newRouter(t)

	form := url.Values{}
	form.Set("email", "alice@acme.test")
	form.Set("password", "pw-alice")
	loginRec := do(t, h, http.MethodPost, "acme.crm.local", "/login", strings.NewReader(form.Encode()))
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status=%d, want 302", loginRec.Code)
	}
	acmeCookie := loginRec.Result().Cookies()[0]

	// Same cookie, different tenant — must be rejected.
	rec := do(t, h, http.MethodGet, "globex.crm.local", "/hello-tenant", nil, acmeCookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("cross-tenant probe status=%d, want 302 (or 401); MUST not return 200", rec.Code)
	}
	if got := rec.Header().Get("Location"); !strings.HasPrefix(got, "/login") {
		t.Fatalf("cross-tenant Location=%q, want /login* prefix", got)
	}
}

func TestRouter_LoginPost_WrongPassword_ReRendersForm(t *testing.T) {
	t.Parallel()
	h, _, _ := newRouter(t)

	form := url.Values{}
	form.Set("email", "alice@acme.test")
	form.Set("password", "wrong")
	rec := do(t, h, http.MethodPost, "acme.crm.local", "/login", strings.NewReader(form.Encode()))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Email ou senha inválidos.") {
		t.Fatalf("body missing generic error: %q", rec.Body.String())
	}
	if cs := rec.Result().Cookies(); len(cs) != 0 {
		t.Fatalf("got %d cookies on auth failure, want 0", len(cs))
	}
}

// TestRouter_Logout_GETReturns405_PreservesSession is SIN-62419 AC #4:
// after the GET-side logout surface was removed (post PR #43), a GET to
// /logout — the exact attacker shape `<img src="/logout">` — must NOT
// terminate the session. chi returns 405 (the path exists for POST in
// the authed group), no Set-Cookie header clears the session, and the
// same session cookie still authenticates GET /hello-tenant afterwards.
//
// The success path (POST /logout with a valid CSRF token + cookies) is
// covered by TestRouter_CSRF_LogoutOriginAllowlist_AcceptsTenantHost in
// router_csrf_test.go; this test pins the *negative* CSRF-out shape.
func TestRouter_Logout_GETReturns405_PreservesSession(t *testing.T) {
	t.Parallel()
	h, _, _ := newRouter(t)

	form := url.Values{}
	form.Set("email", "alice@acme.test")
	form.Set("password", "pw-alice")
	loginRec := do(t, h, http.MethodPost, "acme.crm.local", "/login", strings.NewReader(form.Encode()))
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status=%d, want 302", loginRec.Code)
	}
	cookie := loginRec.Result().Cookies()[0]

	rec := do(t, h, http.MethodGet, "acme.crm.local", "/logout", nil, cookie)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", rec.Code)
	}
	if got := rec.Result().Cookies(); len(got) != 0 {
		t.Fatalf("GET /logout must not set/clear cookies, got %d: %+v", len(got), got)
	}

	// Same cookie after the (rejected) GET /logout must still authenticate —
	// the session was never terminated.
	hello := do(t, h, http.MethodGet, "acme.crm.local", "/hello-tenant", nil, cookie)
	if hello.Code != http.StatusOK {
		t.Fatalf("post-rejected-GET hello status=%d, want 200 (session must still be valid)", hello.Code)
	}
}

func TestNewRouter_PanicsOnMissingDeps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		deps httpapi.Deps
	}{
		{"nil IAM", httpapi.Deps{IAM: nil, TenantResolver: &fakeResolver{}}},
		{"nil resolver", httpapi.Deps{IAM: newInmemIAM(map[string]uuid.UUID{}), TenantResolver: nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic")
				}
			}()
			httpapi.NewRouter(tc.deps)
		})
	}
}

func TestRouter_TenantScopeInfraError_500(t *testing.T) {
	t.Parallel()
	r := httpapi.NewRouter(httpapi.Deps{
		IAM:            newInmemIAM(map[string]uuid.UUID{}),
		TenantResolver: failingResolver{},
	})
	rec := do(t, r, http.MethodGet, "acme.crm.local", "/login", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 (resolver infra error)", rec.Code)
	}
}

type failingResolver struct{}

func (failingResolver) ResolveByHost(context.Context, string) (*tenancy.Tenant, error) {
	return nil, errors.New("db down")
}
