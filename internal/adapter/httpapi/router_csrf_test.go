package httpapi_test

// SIN-62375 / FAIL-2 of SIN-62343 — RequireCSRF integration tests.
//
// Each test boots the real router (NewRouter), seeds a CSRF token onto
// the in-memory IAM store, drives a POST /logout request through the
// authenticated chain, and asserts the documented rejection / accept
// path. ADR 0073 §D1 fixes the rejection-reason vocabulary; the tests
// pin those names via the OnReject callback so a future rename of the
// labels (which feed Prometheus dashboards) is caught at the test layer
// instead of silently dropping a metrics dimension.

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
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// csrfIAM is a router-level IAM fake that, unlike inmemIAM in
// router_test.go, mints a deterministic CSRF token on Login and echoes
// it back on ValidateSession so the integration test can mirror the
// production flow without dragging the real iam.Service in.
type csrfIAM struct {
	mu        sync.Mutex
	tenants   map[string]uuid.UUID
	users     map[string]string // host|email -> password
	sessions  map[uuid.UUID]iam.Session
	csrfToken string
}

func newCSRFIAM(tenants map[string]uuid.UUID, csrfToken string) *csrfIAM {
	return &csrfIAM{
		tenants:   tenants,
		users:     map[string]string{},
		sessions:  map[uuid.UUID]iam.Session{},
		csrfToken: csrfToken,
	}
}

func (s *csrfIAM) addUser(host, email, password string) {
	s.users[host+"|"+email] = password
}

func (s *csrfIAM) Login(_ context.Context, host, email, password string, _ net.IP, _, _ string) (iam.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tenantID, ok := s.tenants[host]
	if !ok {
		return iam.Session{}, iam.ErrInvalidCredentials
	}
	if s.users[host+"|"+email] != password {
		return iam.Session{}, iam.ErrInvalidCredentials
	}
	now := time.Now().UTC()
	sess := iam.Session{
		ID:        uuid.New(),
		UserID:    uuid.New(),
		TenantID:  tenantID,
		ExpiresAt: now.Add(time.Hour),
		CreatedAt: now,
		CSRFToken: s.csrfToken,
	}
	s.sessions[sess.ID] = sess
	return sess, nil
}

func (s *csrfIAM) Logout(_ context.Context, _, sessionID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
	return nil
}

func (s *csrfIAM) ValidateSession(_ context.Context, tenantID, sessionID uuid.UUID) (iam.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok || sess.TenantID != tenantID {
		return iam.Session{}, iam.ErrSessionNotFound
	}
	if time.Now().After(sess.ExpiresAt) {
		return iam.Session{}, iam.ErrSessionExpired
	}
	return sess, nil
}

// csrfRecorder captures every reason emitted by the RequireCSRF
// middleware. We assert against the exact label set documented in
// ADR 0073 §D1 — these strings flow into Prometheus dashboards, so a
// silent rename is a regression we want caught here.
type csrfRecorder struct {
	mu      sync.Mutex
	reasons []csrfmw.Reason
}

func (r *csrfRecorder) Record(_ *http.Request, reason csrfmw.Reason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reasons = append(r.reasons, reason)
}

func (r *csrfRecorder) Last() csrfmw.Reason {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reasons) == 0 {
		return ""
	}
	return r.reasons[len(r.reasons)-1]
}

// loginAndCookies signs alice@acme.test in and returns the resulting
// __Host-sess-tenant + __Host-csrf cookies. Tests reuse this so each
// CSRF assertion starts from a realistic post-login state (both
// cookies set) rather than hand-forging cookies that would diverge
// from the production wire format.
func loginAndCookies(t *testing.T, h http.Handler, host string) (sessionCookie, csrfCookie *http.Cookie) {
	t.Helper()
	form := url.Values{}
	form.Set("email", "alice@acme.test")
	form.Set("password", "pw-alice")
	loginRec := do(t, h, http.MethodPost, host, "/login", strings.NewReader(form.Encode()))
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status=%d, want 302", loginRec.Code)
	}
	for _, c := range loginRec.Result().Cookies() {
		switch c.Name {
		case sessioncookie.NameTenant:
			sessionCookie = c
		case sessioncookie.NameCSRF:
			csrfCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatalf("login did not set %s cookie", sessioncookie.NameTenant)
	}
	if csrfCookie == nil {
		t.Fatalf("login did not set %s cookie", sessioncookie.NameCSRF)
	}
	return sessionCookie, csrfCookie
}

func newCSRFRouter(t *testing.T, csrfToken string, recorder *csrfRecorder) (http.Handler, *csrfIAM) {
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
	})
	return r, iamFake
}

// postLogoutWith fires POST /logout with caller-supplied headers and
// cookies. It mirrors the helper `do` shape but accepts a header bag
// so the Origin/Referer + X-CSRF-Token tests stay readable.
func postLogoutWith(t *testing.T, h http.Handler, host string, headers map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(""))
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

// TestRouter_CSRF_LogoutMissingHeader asserts ADR 0073 §D1 step 4 —
// a POST that carries the cookie but no X-CSRF-Token / form field is
// rejected with 403 and reason "csrf.token_missing". Cookie-only
// presentation is *exactly* the attacker shape (cross-site form post
// without HTMX wiring) we want to fail.
func TestRouter_CSRF_LogoutMissingHeader(t *testing.T) {
	t.Parallel()
	const csrfToken = "test-csrf-token-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	recorder := &csrfRecorder{}
	h, _ := newCSRFRouter(t, csrfToken, recorder)
	const host = "acme.crm.local"

	sess, csrfCookie := loginAndCookies(t, h, host)

	rec := postLogoutWith(t, h, host, map[string]string{
		"Origin": "https://" + host,
	}, sess, csrfCookie)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	if got := recorder.Last(); got != csrfmw.ReasonTokenMissing {
		t.Fatalf("reason=%q, want %q", got, csrfmw.ReasonTokenMissing)
	}
}

// TestRouter_CSRF_LogoutTokenMismatch asserts ADR 0073 §D1 step 5 —
// header value differs from the cookie value (subdomain takeover
// scenario) → 403 with reason csrf.token_mismatch.
func TestRouter_CSRF_LogoutTokenMismatch(t *testing.T) {
	t.Parallel()
	const csrfToken = "test-csrf-token-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	recorder := &csrfRecorder{}
	h, _ := newCSRFRouter(t, csrfToken, recorder)
	const host = "acme.crm.local"

	sess, csrfCookie := loginAndCookies(t, h, host)

	rec := postLogoutWith(t, h, host, map[string]string{
		"Origin":          "https://" + host,
		csrfmw.HeaderName: "totally-different-value",
	}, sess, csrfCookie)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	if got := recorder.Last(); got != csrfmw.ReasonTokenMismatch {
		t.Fatalf("reason=%q, want %q", got, csrfmw.ReasonTokenMismatch)
	}
}

// TestRouter_CSRF_LogoutOriginMismatch asserts the independent
// Origin/Referer allowlist layer (ADR 0073 §D1 final paragraph). Even
// with a fully valid cookie + header pair, an off-list Origin → 403
// with reason csrf.origin_mismatch. This is the "stolen token + stolen
// cookie still cannot be replayed" defense.
func TestRouter_CSRF_LogoutOriginMismatch(t *testing.T) {
	t.Parallel()
	const csrfToken = "test-csrf-token-cccccccccccccccccccccccccccccccccccc"
	recorder := &csrfRecorder{}
	h, _ := newCSRFRouter(t, csrfToken, recorder)
	const host = "acme.crm.local"

	sess, csrfCookie := loginAndCookies(t, h, host)

	rec := postLogoutWith(t, h, host, map[string]string{
		"Origin":          "https://attacker.example",
		csrfmw.HeaderName: csrfToken,
	}, sess, csrfCookie)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	if got := recorder.Last(); got != csrfmw.ReasonOriginMismatch {
		t.Fatalf("reason=%q, want %q", got, csrfmw.ReasonOriginMismatch)
	}
}

// TestRouter_CSRF_LogoutOriginAllowlist_AcceptsTenantHost is the
// success path: cookie + matching header + same-origin POST → the
// logout handler runs, returning 302 to /login. This proves the
// allowlist closure (csrfAllowedHosts) returns the resolved tenant
// host; without it, every legitimate logout would 403.
func TestRouter_CSRF_LogoutOriginAllowlist_AcceptsTenantHost(t *testing.T) {
	t.Parallel()
	const csrfToken = "test-csrf-token-dddddddddddddddddddddddddddddddddddd"
	recorder := &csrfRecorder{}
	h, _ := newCSRFRouter(t, csrfToken, recorder)
	const host = "acme.crm.local"

	sess, csrfCookie := loginAndCookies(t, h, host)

	rec := postLogoutWith(t, h, host, map[string]string{
		"Origin":          "https://" + host,
		csrfmw.HeaderName: csrfToken,
	}, sess, csrfCookie)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302 (logout success); recorder=%v", rec.Code, recorder.reasons)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("Location=%q, want /login", loc)
	}
	// No CSRF rejection should have been recorded on the success path.
	if len(recorder.reasons) != 0 {
		t.Fatalf("recorder captured rejections on success path: %v", recorder.reasons)
	}
}

// TestRouter_CSRF_LogoutOriginAllowlist_AcceptsMasterHost asserts the
// MasterHost slot of csrfAllowedHosts: a POST whose Origin points at
// the configured master.crm.local is accepted alongside the resolved
// tenant host. This codifies the ADR 0073 §D1 "[masterHost, tenantHost]"
// list shape.
func TestRouter_CSRF_LogoutOriginAllowlist_AcceptsMasterHost(t *testing.T) {
	t.Parallel()
	const csrfToken = "test-csrf-token-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	recorder := &csrfRecorder{}
	h, _ := newCSRFRouter(t, csrfToken, recorder)
	const host = "acme.crm.local"

	sess, csrfCookie := loginAndCookies(t, h, host)

	rec := postLogoutWith(t, h, host, map[string]string{
		"Origin":          "https://master.crm.local",
		csrfmw.HeaderName: csrfToken,
	}, sess, csrfCookie)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302; recorder=%v", rec.Code, recorder.reasons)
	}
}

// TestRouter_CSRF_LogoutNoCSRFCookie asserts ADR 0073 §D1 step 3 —
// missing __Host-csrf cookie → 403 with reason csrf.cookie_missing.
// We send only the session cookie; the CSRF cookie is intentionally
// absent (e.g. attacker page that never could read it because it's
// SameSite=Strict from a different site).
func TestRouter_CSRF_LogoutNoCSRFCookie(t *testing.T) {
	t.Parallel()
	const csrfToken = "test-csrf-token-ffffffffffffffffffffffffffffffffffff"
	recorder := &csrfRecorder{}
	h, _ := newCSRFRouter(t, csrfToken, recorder)
	const host = "acme.crm.local"

	sess, _ := loginAndCookies(t, h, host)

	rec := postLogoutWith(t, h, host, map[string]string{
		"Origin": "https://" + host,
	}, sess)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	if got := recorder.Last(); got != csrfmw.ReasonCookieMissing {
		t.Fatalf("reason=%q, want %q", got, csrfmw.ReasonCookieMissing)
	}
}

// TestRouter_CSRF_LogoutMissingOrigin asserts that a POST without any
// Origin or Referer header — even with a perfectly matching cookie +
// header pair — is rejected with csrf.origin_missing. ADR 0073 §D1
// requires both layers; we cannot accept a presented token alone if
// we cannot also pin where the request came from.
func TestRouter_CSRF_LogoutMissingOrigin(t *testing.T) {
	t.Parallel()
	const csrfToken = "test-csrf-token-1111111111111111111111111111111111"
	recorder := &csrfRecorder{}
	h, _ := newCSRFRouter(t, csrfToken, recorder)
	const host = "acme.crm.local"

	sess, csrfCookie := loginAndCookies(t, h, host)

	// Origin and Referer both omitted.
	rec := postLogoutWith(t, h, host, map[string]string{
		csrfmw.HeaderName: csrfToken,
	}, sess, csrfCookie)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	if got := recorder.Last(); got != csrfmw.ReasonOriginMissing {
		t.Fatalf("reason=%q, want %q", got, csrfmw.ReasonOriginMissing)
	}
}

// TestRouter_CSRF_HelloTenantBypassedForGet asserts the safe-method
// short circuit (ADR 0073 §D1 step 1). GET /hello-tenant must continue
// to render even with no CSRF cookie / header — otherwise CSRF turns
// into a half-broken auth gate.
func TestRouter_CSRF_HelloTenantBypassedForGet(t *testing.T) {
	t.Parallel()
	const csrfToken = "test-csrf-token-2222222222222222222222222222222222"
	recorder := &csrfRecorder{}
	h, _ := newCSRFRouter(t, csrfToken, recorder)
	const host = "acme.crm.local"

	sess, _ := loginAndCookies(t, h, host)

	// Drop the CSRF cookie deliberately on this GET — safe-method must
	// pass anyway.
	rec := do(t, h, http.MethodGet, host, "/hello-tenant", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /hello-tenant status=%d, want 200; recorder=%v", rec.Code, recorder.reasons)
	}
	if len(recorder.reasons) != 0 {
		t.Fatalf("safe method should not have triggered RequireCSRF: %v", recorder.reasons)
	}
}

// TestRouter_CSRF_LoginCookieFlags pins the __Host-csrf wire format
// (ADR 0073 §D1 cookie row): __Host- prefix, Secure, Path=/,
// HttpOnly=false (HTMX must read it), SameSite=Strict. A drift in any
// of these flags is a security regression we want caught here, not in
// production.
func TestRouter_CSRF_LoginCookieFlags(t *testing.T) {
	t.Parallel()
	const csrfToken = "test-csrf-token-3333333333333333333333333333333333"
	h, _ := newCSRFRouter(t, csrfToken, &csrfRecorder{})
	const host = "acme.crm.local"

	_, csrfCookie := loginAndCookies(t, h, host)
	if csrfCookie.Name != sessioncookie.NameCSRF {
		t.Fatalf("name=%q, want %q", csrfCookie.Name, sessioncookie.NameCSRF)
	}
	if csrfCookie.Value != csrfToken {
		t.Fatalf("value=%q, want %q", csrfCookie.Value, csrfToken)
	}
	if !csrfCookie.Secure {
		t.Fatal("CSRF cookie missing Secure")
	}
	if csrfCookie.HttpOnly {
		t.Fatal("CSRF cookie HttpOnly must be false (HTMX reads it)")
	}
	if csrfCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("SameSite=%v, want Strict", csrfCookie.SameSite)
	}
	if csrfCookie.Path != "/" {
		t.Fatalf("Path=%q, want /", csrfCookie.Path)
	}
}
