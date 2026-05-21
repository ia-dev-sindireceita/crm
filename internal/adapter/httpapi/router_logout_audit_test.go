package httpapi_test

// SIN-63214 — wire-level proof that the tenant POST /logout route
// constructed by httpapi.NewRouter passes the Deps.AuditLogger through
// the handler.WithLogoutAudit option added in PR #234 / SIN-63188.
//
// The PR234 package tests in internal/adapter/httpapi/handler already
// assert the handler emits the right SecurityAuditEvent shape; this
// file pins the wiring seam at the router level so a future Deps
// refactor that drops the option silently is caught here.

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	csrfmw "github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// captorSplitLogger records every WriteSecurity / WriteData call so the
// test can assert the SecurityEventLogout row produced by /logout. The
// optional writeSecurityErr lets a test drive the best-effort path:
// the handler MUST log the error and continue with the redirect.
type captorSplitLogger struct {
	mu               sync.Mutex
	securityCalls    []audit.SecurityAuditEvent
	dataCalls        []audit.DataAuditEvent
	writeSecurityErr error
}

func (c *captorSplitLogger) WriteSecurity(_ context.Context, e audit.SecurityAuditEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.securityCalls = append(c.securityCalls, e)
	return c.writeSecurityErr
}

func (c *captorSplitLogger) WriteData(_ context.Context, e audit.DataAuditEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dataCalls = append(c.dataCalls, e)
	return nil
}

func (c *captorSplitLogger) securitySnapshot() []audit.SecurityAuditEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]audit.SecurityAuditEvent, len(c.securityCalls))
	copy(out, c.securityCalls)
	return out
}

// newCSRFRouterWithAudit mirrors newCSRFRouter (router_csrf_test.go)
// but additionally wires Deps.AuditLogger so the SIN-63214 wireup is
// exercised end-to-end. The tenant id is returned so tests can compare
// it against the audit row's TenantID pointer.
func newCSRFRouterWithAudit(t *testing.T, csrfToken string, audit audit.SplitLogger) (http.Handler, *csrfIAM, uuid.UUID) {
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
		IAM:            iamFake,
		TenantResolver: resolver,
		MasterHost:     "master.crm.local",
		AuditLogger:    audit,
	})
	return r, iamFake, acmeID
}

// TestRouter_Logout_EmitsSecurityEventLogout drives the success path:
// POST /logout with valid session + CSRF cookies → handler.Logout runs,
// deletes the in-memory session, and (via the SIN-63214 wireup) appends
// a SecurityEventLogout row with audience=tenant to the audit logger.
//
// The PR234 handler test already pins the row shape (target keys,
// best-effort error path); this test only proves the wireup. Asserting
// the event + audience is enough to fail loudly if Deps.AuditLogger is
// dropped from the option list in router.go.
func TestRouter_Logout_EmitsSecurityEventLogout(t *testing.T) {
	t.Parallel()
	const csrfToken = "test-csrf-token-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	captor := &captorSplitLogger{}
	h, _, tenantID := newCSRFRouterWithAudit(t, csrfToken, captor)
	const host = "acme.crm.local"

	sess, csrfCookie := loginAndCookies(t, h, host)

	rec := postLogoutWith(t, h, host, map[string]string{
		"Origin":          "https://" + host,
		csrfmw.HeaderName: csrfToken,
	}, sess, csrfCookie)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302 (logout success)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("Location=%q, want /login", loc)
	}

	calls := captor.securitySnapshot()
	if len(calls) != 1 {
		t.Fatalf("WriteSecurity calls=%d, want 1; got %+v", len(calls), calls)
	}
	if calls[0].Event != audit.SecurityEventLogout {
		t.Fatalf("event=%s, want %s", calls[0].Event, audit.SecurityEventLogout)
	}
	if calls[0].TenantID == nil || *calls[0].TenantID != tenantID {
		t.Fatalf("tenant_id=%v, want %v", calls[0].TenantID, tenantID)
	}
	if got, _ := calls[0].Target["audience"].(string); got != "tenant" {
		t.Fatalf("target.audience=%q, want %q", got, "tenant")
	}
	if _, ok := calls[0].Target["session_id"].(string); !ok {
		t.Fatalf("target.session_id missing or wrong type: %+v", calls[0].Target)
	}
}

// TestRouter_Logout_AuditWriteFailureStillRedirects pins the best-effort
// contract: a SplitLogger that errors on WriteSecurity MUST NOT prevent
// the redirect or the cookie clear. ADR 0073 §D3 — once the user has
// clicked log out, the server cannot decide to keep them logged in.
func TestRouter_Logout_AuditWriteFailureStillRedirects(t *testing.T) {
	t.Parallel()
	const csrfToken = "test-csrf-token-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	captor := &captorSplitLogger{writeSecurityErr: errors.New("synthetic audit failure")}
	h, _, _ := newCSRFRouterWithAudit(t, csrfToken, captor)
	const host = "acme.crm.local"

	sess, csrfCookie := loginAndCookies(t, h, host)

	rec := postLogoutWith(t, h, host, map[string]string{
		"Origin":          "https://" + host,
		csrfmw.HeaderName: csrfToken,
	}, sess, csrfCookie)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302 despite audit write failure", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("Location=%q, want /login", loc)
	}
	// The handler still calls WriteSecurity once; the error is swallowed
	// (logged) so the user-visible redirect proceeds.
	if got := len(captor.securitySnapshot()); got != 1 {
		t.Fatalf("WriteSecurity calls=%d, want 1", got)
	}
}

// TestRouter_Logout_NilAuditLoggerSkipsWrite proves the backward-compat
// path: Deps.AuditLogger left at the zero value means the handler runs
// in its pre-PR234 shape — no audit write, redirect still fires. This
// is the property router_csrf_test.go relies on; pinning it here keeps
// the option list nil-safe across future refactors.
func TestRouter_Logout_NilAuditLoggerSkipsWrite(t *testing.T) {
	t.Parallel()
	const csrfToken = "test-csrf-token-cccccccccccccccccccccccccccccccccccc"
	// nil AuditLogger - rely on the WithLogoutAudit(nil) short-circuit
	// inside the handler.
	h, _, _ := newCSRFRouterWithAudit(t, csrfToken, nil)
	const host = "acme.crm.local"

	sess, csrfCookie := loginAndCookies(t, h, host)

	rec := postLogoutWith(t, h, host, map[string]string{
		"Origin":          "https://" + host,
		csrfmw.HeaderName: csrfToken,
	}, sess, csrfCookie)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
}
