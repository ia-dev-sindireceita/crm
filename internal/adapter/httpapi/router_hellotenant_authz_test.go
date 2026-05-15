package httpapi_test

// SIN-62767 — first protected production route is gated on
// RequireAction(audited, ActionTenantContactRead). These tests assert
// the full router → Auth → RequireAuth → RequireAction → audited
// Recorder chain fires at /hello-tenant: deny on empty / cross-role
// session minted by login is 403 + a Recorder.Record (deny at 100%);
// allow on a tenant-role session is 200 + a Recorder.Record (allow at
// sampler=Always). The existing role-less inmemIAM in router_test.go
// is reused for the deny case; a tiny role-aware helper covers the
// allow case without modifying any existing fixture.

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/authz"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// authzRecord captures one Recorder.Record call so the test can
// assert the (principal, action, decision) tuple end-to-end.
type authzRecord struct {
	principal iam.Principal
	action    iam.Action
	resource  iam.Resource
	decision  iam.Decision
}

// authzRecorder is a Recorder that captures every call. Concurrent
// writes are serialised on a single mutex; the router test never
// fires parallel requests against the same recorder, but the lock
// keeps the helper safe for any future addition.
type authzRecorder struct {
	mu      sync.Mutex
	records []authzRecord
}

func (r *authzRecorder) Record(_ context.Context, p iam.Principal, a iam.Action, res iam.Resource, d iam.Decision, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, authzRecord{principal: p, action: a, resource: res, decision: d})
}

func (r *authzRecorder) snapshot() []authzRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]authzRecord, len(r.records))
	copy(out, r.records)
	return out
}

// roledIAM is a minimal in-memory IAMService whose Login mints a
// Session carrying a specific Role, so the Principal derived from
// it exercises the production RBAC matrix end-to-end. It is
// independent of the role-less inmemIAM in router_test.go because
// the existing helper does not set Session.Role — and the SIN-62767
// quality bar forbids modifying existing test fixtures without CTO
// authorization.
type roledIAM struct {
	mu       sync.Mutex
	tenants  map[string]uuid.UUID
	users    map[string]roledUser
	sessions map[uuid.UUID]map[uuid.UUID]roledSession
}

type roledUser struct {
	tenantID uuid.UUID
	userID   uuid.UUID
	password string
	role     iam.Role
}

type roledSession struct {
	id       uuid.UUID
	userID   uuid.UUID
	tenantID uuid.UUID
	role     iam.Role
	expires  time.Time
}

func newRoledIAM(tenants map[string]uuid.UUID) *roledIAM {
	return &roledIAM{
		tenants:  tenants,
		users:    map[string]roledUser{},
		sessions: map[uuid.UUID]map[uuid.UUID]roledSession{},
	}
}

func (s *roledIAM) addUser(host, email, password string, role iam.Role, userID uuid.UUID) {
	s.users[host+"|"+email] = roledUser{
		tenantID: s.tenants[host],
		userID:   userID,
		password: password,
		role:     role,
	}
}

func (s *roledIAM) Login(_ context.Context, host, email, password string, _ net.IP, _, _ string) (iam.Session, error) {
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
	sess := roledSession{
		id:       id,
		userID:   u.userID,
		tenantID: tenantID,
		role:     u.role,
		expires:  time.Now().Add(time.Hour),
	}
	if s.sessions[tenantID] == nil {
		s.sessions[tenantID] = map[uuid.UUID]roledSession{}
	}
	s.sessions[tenantID][id] = sess
	return iam.Session{
		ID:        id,
		UserID:    u.userID,
		TenantID:  tenantID,
		Role:      u.role,
		ExpiresAt: sess.expires,
	}, nil
}

func (s *roledIAM) Logout(_ context.Context, tenantID, sessionID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.sessions[tenantID]; ok {
		delete(m, sessionID)
	}
	return nil
}

func (s *roledIAM) ValidateSession(_ context.Context, tenantID, sessionID uuid.UUID) (iam.Session, error) {
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
	return iam.Session{
		ID:        sess.id,
		UserID:    sess.userID,
		TenantID:  sess.tenantID,
		Role:      sess.role,
		ExpiresAt: sess.expires,
	}, nil
}

// authzRouterDeps builds a router whose Authorizer field is the
// SIN-62254 AuditingAuthorizer wrapping the production RBAC matrix,
// with the given Sampler and a fresh authzRecorder. The recorder is
// returned so tests can assert on captured Decisions.
func authzRouterDeps(t *testing.T, iamSvc httpapi.IAMService, resolver tenancy.Resolver, sampler authz.Sampler) (http.Handler, *authzRecorder) {
	t.Helper()
	rec := &authzRecorder{}
	audited := authz.New(authz.Config{
		Inner:    iam.NewRBACAuthorizer(iam.RBACConfig{}),
		Recorder: rec,
		Sampler:  sampler,
	})
	h := httpapi.NewRouter(httpapi.Deps{
		IAM:            iamSvc,
		TenantResolver: resolver,
		Authorizer:     audited,
	})
	return h, rec
}

// loginCookie POSTs the form-encoded credentials, asserts the 302
// + session cookie return, and returns the cookie. Pulled out so
// allow/deny tests share the bootstrap path verbatim.
func loginCookie(t *testing.T, h http.Handler, host, email, password string) *http.Cookie {
	t.Helper()
	form := "email=" + email + "&password=" + password
	rec := do(t, h, http.MethodPost, host, "/login", strings.NewReader(form))
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302 (body=%q)", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != middleware.SessionCookieName {
		t.Fatalf("login did not set the session cookie: %+v", cookies)
	}
	return cookies[0]
}

// TestRouter_HelloTenant_Authz_DenyOnEmptyRoleSession is the F10
// horizontal-probing capture path: a session with no role (or any
// role outside the matrix entry for ActionTenantContactRead) reaches
// /hello-tenant and is denied at the RequireAction gate with 403,
// AND the audited Recorder sees one deny record. This is the deny
// half of the AC: "one real 403 produces ... one increment on both
// authz_decisions_total{outcome=\"deny\"} and authz_user_deny_total".
func TestRouter_HelloTenant_Authz_DenyOnEmptyRoleSession(t *testing.T) {
	t.Parallel()
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": acmeID}
	store := newInmemIAM(tenantIDs)
	store.addUser("acme.crm.local", "alice@acme.test", "pw-alice", uuid.New())
	resolver := &fakeResolver{byHost: tenants}

	h, recorder := authzRouterDeps(t, store, resolver, authz.NeverSample{})

	cookie := loginCookie(t, h, "acme.crm.local", "alice@acme.test", "pw-alice")
	rec := do(t, h, http.MethodGet, "acme.crm.local", "/hello-tenant", nil, cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (empty-role session must be denied at RequireAction)", rec.Code)
	}

	got := recorder.snapshot()
	if len(got) != 1 {
		t.Fatalf("recorder captured %d records, want 1 (deny is 100%%)", len(got))
	}
	if got[0].decision.Allow {
		t.Fatalf("recorder captured an allow on a 403 path: %+v", got[0])
	}
	if got[0].action != iam.ActionTenantContactRead {
		t.Fatalf("recorded action = %q, want %q", got[0].action, iam.ActionTenantContactRead)
	}
	// Empty Role on the session translates to a Principal whose role
	// set does not intersect the matrix entry for the action — the
	// RBAC authorizer returns ReasonDeniedRBAC, which is the closed-
	// set reason code the audit row will carry.
	if got[0].decision.ReasonCode != iam.ReasonDeniedRBAC {
		t.Fatalf("reason_code = %q, want %q", got[0].decision.ReasonCode, iam.ReasonDeniedRBAC)
	}
}

// TestRouter_HelloTenant_Authz_AllowOnTenantRoleSession is the
// allow half of the AC: a session minted with a tenant role
// (RoleTenantCommon — the most-permissive entry for the action)
// reaches /hello-tenant, the handler renders 200, and the audited
// Recorder captures the allow because the sampler is set to Always.
// The 1% production sampler is exercised in authz/sampler_test.go;
// here we lock the wireup contract.
func TestRouter_HelloTenant_Authz_AllowOnTenantRoleSession(t *testing.T) {
	t.Parallel()
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": acmeID}
	store := newRoledIAM(tenantIDs)
	store.addUser("acme.crm.local", "alice@acme.test", "pw-alice", iam.RoleTenantCommon, uuid.New())
	resolver := &fakeResolver{byHost: tenants}

	h, recorder := authzRouterDeps(t, store, resolver, authz.AlwaysSample{})

	cookie := loginCookie(t, h, "acme.crm.local", "alice@acme.test", "pw-alice")
	rec := do(t, h, http.MethodGet, "acme.crm.local", "/hello-tenant", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (tenant-common role must be allowed on tenant.contact.read; body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "acme") {
		t.Fatalf("body missing tenant name: %q", rec.Body.String())
	}

	got := recorder.snapshot()
	if len(got) != 1 {
		t.Fatalf("recorder captured %d records, want 1 (sampler=Always)", len(got))
	}
	if !got[0].decision.Allow {
		t.Fatalf("recorder captured a deny on a 200 path: %+v", got[0])
	}
	if got[0].action != iam.ActionTenantContactRead {
		t.Fatalf("recorded action = %q, want %q", got[0].action, iam.ActionTenantContactRead)
	}
	if got[0].decision.ReasonCode != iam.ReasonAllowedRBAC {
		t.Fatalf("reason_code = %q, want %q", got[0].decision.ReasonCode, iam.ReasonAllowedRBAC)
	}
}

// TestRouter_HelloTenant_Authz_AllowDroppedWhenSamplerSaysNo locks
// the deny-100%/allow-sampled retention contract: an allow at
// sampler=Never produces 200 (decision invariance — the verdict is
// unchanged by the sampler) AND ZERO Recorder records (no audit
// row for the unsampled allow).
func TestRouter_HelloTenant_Authz_AllowDroppedWhenSamplerSaysNo(t *testing.T) {
	t.Parallel()
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": acmeID}
	store := newRoledIAM(tenantIDs)
	store.addUser("acme.crm.local", "bob@acme.test", "pw-bob", iam.RoleTenantAtendente, uuid.New())
	resolver := &fakeResolver{byHost: tenants}

	h, recorder := authzRouterDeps(t, store, resolver, authz.NeverSample{})

	cookie := loginCookie(t, h, "acme.crm.local", "bob@acme.test", "pw-bob")
	rec := do(t, h, http.MethodGet, "acme.crm.local", "/hello-tenant", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (sampler must not change the verdict)", rec.Code)
	}
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("recorder captured %d records, want 0 (sampler=Never drops allows)", len(got))
	}
}

// TestRouter_HelloTenant_Authz_NoAuthorizer_BackwardCompat documents
// the conditional-mount contract: when Deps.Authorizer is nil the
// router mounts /hello-tenant without RequireAction so existing
// router_test.go scenarios (which don't supply an Authorizer) keep
// reaching the handler unchanged. The full role-less login flow
// asserted in TestRouter_LoginPost_ThenHelloTenant_BodyContainsTenantName
// already covers the positive case; this test focuses on the deny
// path that would have fired with an Authorizer wired, proving it
// does NOT fire when the seam is nil.
func TestRouter_HelloTenant_Authz_NoAuthorizer_BackwardCompat(t *testing.T) {
	t.Parallel()
	h, _, _ := newRouter(t)

	cookie := loginCookie(t, h, "acme.crm.local", "alice@acme.test", "pw-alice")
	rec := do(t, h, http.MethodGet, "acme.crm.local", "/hello-tenant", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Deps.Authorizer nil ⇒ no RequireAction gate)", rec.Code)
	}
}
