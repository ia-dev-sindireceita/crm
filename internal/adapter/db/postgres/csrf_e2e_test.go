package postgres_test

// SIN-63222 end-to-end CSRF regression. Drives the real httpapi.NewRouter
// chain against a Postgres-backed iam.Service so a future regression of
// the adapter ↔ schema CSRF wireup (the SIN-63222 root cause) is caught
// here at the smoke level, not in production.
//
// The existing router_csrf_test.go suite uses an in-memory IAM fake that
// stores CSRFToken in a map, so it never exercises the SQL round-trip
// that broke. This file fills the gap: it boots iam.Service with the
// real postgres SessionStore + UserCredentialReader, drives
// POST /login → captures cookies → POST /logout with X-CSRF-Token, and
// asserts the 302 redirect. Before the fix the second request 403'd
// with reason "csrf.token_missing" because SessionStore.Get returned an
// empty CSRFToken; after the fix it round-trips through migration 0111's
// csrf_token column and the middleware passes.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	csrfmw "github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// tenancyResolver is a minimal tenancy.Resolver that returns one tenant
// for a single configured host. Used by the router; iam.Service has its
// own iam.TenantResolver (fixedTenantResolver in session_store_test.go).
type tenancyResolver struct {
	host   string
	tenant *tenancy.Tenant
}

func (r tenancyResolver) ResolveByHost(_ context.Context, host string) (*tenancy.Tenant, error) {
	if host != r.host {
		return nil, tenancy.ErrTenantNotFound
	}
	return r.tenant, nil
}

// rejectRecorder captures CSRF rejection reasons so a failure surfaces
// the exact middleware path that misfired (token_missing vs token_mismatch
// vs origin_mismatch). Mirrors the recorder shape from router_csrf_test.go.
type rejectRecorder struct {
	mu      sync.Mutex
	reasons []csrfmw.Reason
}

func (r *rejectRecorder) Record(_ *http.Request, reason csrfmw.Reason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reasons = append(r.reasons, reason)
}

func (r *rejectRecorder) snapshot() []csrfmw.Reason {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]csrfmw.Reason, len(r.reasons))
	copy(out, r.reasons)
	return out
}

// TestRouter_CSRF_E2E_PostgresLoginThenLogout is the SIN-63222 smoke E2E.
// Without migration 0111 + the matching SessionStore.Create/Get patch,
// POST /logout returns 403 with reason "csrf.token_missing" even when the
// caller presents the freshly-minted __Host-csrf cookie and matching
// X-CSRF-Token header — the bug the pen-test SIN-63190 caught in
// staging acme. With the fix the same flow returns 302 to /login as
// documented in ADR 0073 §D1.
func TestRouter_CSRF_E2E_PostgresLoginThenLogout(t *testing.T) {
	db := freshDBWithIAM(t)
	const host = "acme.crm.local"
	tenantID, _, plaintext := seedTenant(t, db, host, "alice@acme.test")

	svc := &iam.Service{
		Tenants:  fixedTenantResolver{host: host, tenantID: tenantID},
		Users:    postgres.NewUserCredentialReader(db.RuntimePool()),
		Sessions: postgres.NewSessionStore(db.RuntimePool()),
		TTL:      time.Hour,
	}

	recorder := &rejectRecorder{}
	h := httpapi.NewRouter(httpapi.Deps{
		IAM: svc,
		TenantResolver: tenancyResolver{
			host:   host,
			tenant: &tenancy.Tenant{ID: tenantID, Name: "acme", Host: host},
		},
		MasterHost:       "master.crm.local",
		CSRFRejectMetric: recorder.Record,
	})

	// POST /login — form body + Host header. The handler writes the
	// __Host-sess-tenant and __Host-csrf cookies and 302s to
	// /hello-tenant on success.
	form := url.Values{}
	form.Set("email", "alice@acme.test")
	form.Set("password", plaintext)
	loginReq := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	loginReq.Host = host
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRec := httptest.NewRecorder()
	h.ServeHTTP(loginRec, loginReq)

	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status=%d, want 302; body=%s", loginRec.Code, loginRec.Body.String())
	}

	var sessCookie, csrfCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		switch c.Name {
		case sessioncookie.NameTenant:
			sessCookie = c
		case sessioncookie.NameCSRF:
			csrfCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatalf("login did not set %s cookie", sessioncookie.NameTenant)
	}
	if csrfCookie == nil {
		t.Fatalf("login did not set %s cookie", sessioncookie.NameCSRF)
	}
	if csrfCookie.Value == "" {
		t.Fatalf("CSRF cookie value is empty; iam.Login should mint and the handler should mirror it")
	}

	// Sanity check the bug surface directly: the next read MUST return a
	// session whose CSRFToken matches the cookie. Before the fix this is
	// "" — the middleware then fails. After the fix it equals the cookie.
	sessID, err := uuid.Parse(sessCookie.Value)
	if err != nil {
		t.Fatalf("session cookie value is not a uuid: %q", sessCookie.Value)
	}
	got, err := svc.ValidateSession(context.Background(), tenantID, sessID)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if got.CSRFToken != csrfCookie.Value {
		t.Fatalf("Postgres-backed session CSRFToken=%q, want %q (cookie value); this is the SIN-63222 bug", got.CSRFToken, csrfCookie.Value)
	}

	// POST /logout — cookies + matching X-CSRF-Token + same-origin
	// Origin. Pre-fix: 403 with reason "csrf.token_missing". Post-fix:
	// 302 to /login.
	logoutReq := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(""))
	logoutReq.Host = host
	logoutReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	logoutReq.Header.Set("Origin", "https://"+host)
	logoutReq.Header.Set(csrfmw.HeaderName, csrfCookie.Value)
	logoutReq.AddCookie(sessCookie)
	logoutReq.AddCookie(csrfCookie)
	logoutRec := httptest.NewRecorder()
	h.ServeHTTP(logoutRec, logoutReq)

	if logoutRec.Code != http.StatusFound {
		t.Fatalf("logout status=%d, want 302; rejections=%v; body=%s", logoutRec.Code, recorder.snapshot(), logoutRec.Body.String())
	}
	if loc := logoutRec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("logout Location=%q, want /login", loc)
	}
	if reasons := recorder.snapshot(); len(reasons) != 0 {
		t.Fatalf("CSRF middleware rejected on success path: %v", reasons)
	}
}
