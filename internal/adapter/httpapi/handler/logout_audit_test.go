package handler_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/handler"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// captureSplitLogger is the local in-memory audit.SplitLogger used by
// SIN-63188 logout-audit tests. Keeping it in this package (rather than
// importing the mastermfa counterpart) avoids a cross-package dep that
// would only exist for tests.
type captureSplitLogger struct {
	mu               sync.Mutex
	security         []audit.SecurityAuditEvent
	data             []audit.DataAuditEvent
	writeSecurityErr error
}

func (c *captureSplitLogger) WriteSecurity(_ context.Context, e audit.SecurityAuditEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeSecurityErr != nil {
		return c.writeSecurityErr
	}
	c.security = append(c.security, e)
	return nil
}

func (c *captureSplitLogger) WriteData(_ context.Context, e audit.DataAuditEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = append(c.data, e)
	return nil
}

// TestLogout_Audit_NotEmittedWithoutOption guards the backward-compatible
// default: existing callers (handler.Logout(svc)) MUST keep working with
// no audit logger present and emit no audit row.
func TestLogout_Audit_NotEmittedWithoutOption(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	sessID := uuid.New()
	iamFake := &fakeIAM{}

	r := tenantedRequest(t, http.MethodPost, "/logout", nil, &tenancy.Tenant{ID: tenantID})
	r.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: sessID.String()})
	rec := httptest.NewRecorder()

	// No options — equivalent to the pre-PR6 signature.
	handler.Logout(iamFake)(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	if iamFake.logoutCalls != 1 {
		t.Fatalf("Logout calls=%d, want 1", iamFake.logoutCalls)
	}
}

// TestLogout_Audit_EmitsLogoutRow asserts the SIN-63188 happy path: a
// successful tenant logout with WithLogoutAudit appends exactly one
// SecurityEventLogout row carrying the session id, the tenant_id, the
// audience marker, and the reason.
func TestLogout_Audit_EmitsLogoutRow(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	sessID := uuid.New()
	iamFake := &fakeIAM{}
	logger := &captureSplitLogger{}

	r := tenantedRequest(t, http.MethodPost, "/logout", nil, &tenancy.Tenant{ID: tenantID})
	r.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: sessID.String()})
	rec := httptest.NewRecorder()

	handler.Logout(iamFake, handler.WithLogoutAudit(logger))(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	if got := len(logger.security); got != 1 {
		t.Fatalf("audit rows: got %d want 1", got)
	}
	row := logger.security[0]
	if row.Event != audit.SecurityEventLogout {
		t.Errorf("event: got %q want %q", row.Event, audit.SecurityEventLogout)
	}
	if row.TenantID == nil || *row.TenantID != tenantID {
		t.Errorf("tenant_id: got %v want %v", row.TenantID, tenantID)
	}
	// actor_user_id is uuid.Nil for tenant logout per the handler docstring
	// (the LogoutDeleter port does not surface the principal id).
	if row.ActorUserID != uuid.Nil {
		t.Errorf("actor_user_id: got %v want uuid.Nil", row.ActorUserID)
	}
	if got := row.Target["session_id"]; got != sessID.String() {
		t.Errorf("target.session_id: got %v want %v", got, sessID.String())
	}
	if got := row.Target["audience"]; got != "tenant" {
		t.Errorf("target.audience: got %v want tenant", got)
	}
	if got := row.Target["reason"]; got != "user_initiated" {
		t.Errorf("target.reason: got %v want user_initiated", got)
	}
}

// TestLogout_Audit_NoCookieNoAudit asserts the "no cookie -> no
// principal -> no audit row" branch. The cookie clear + redirect
// still happen.
func TestLogout_Audit_NoCookieNoAudit(t *testing.T) {
	t.Parallel()
	iamFake := &fakeIAM{}
	logger := &captureSplitLogger{}

	r := tenantedRequest(t, http.MethodPost, "/logout", nil, &tenancy.Tenant{ID: uuid.New()})
	rec := httptest.NewRecorder()

	handler.Logout(iamFake, handler.WithLogoutAudit(logger))(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	if len(logger.security) != 0 {
		t.Errorf("audit rows emitted with no cookie: %d", len(logger.security))
	}
}

// TestLogout_Audit_BadCookieNoAudit asserts the "cookie present but
// unparseable" branch does not emit an audit row.
func TestLogout_Audit_BadCookieNoAudit(t *testing.T) {
	t.Parallel()
	iamFake := &fakeIAM{}
	logger := &captureSplitLogger{}

	r := tenantedRequest(t, http.MethodPost, "/logout", nil, &tenancy.Tenant{ID: uuid.New()})
	r.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: "not-a-uuid"})
	rec := httptest.NewRecorder()

	handler.Logout(iamFake, handler.WithLogoutAudit(logger))(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	if len(logger.security) != 0 {
		t.Errorf("audit rows emitted for bad cookie: %d", len(logger.security))
	}
}

// TestLogout_Audit_WriteFailureStillRedirects guards the "audit is
// best-effort" invariant: a SplitLogger that returns an error MUST NOT
// block the cookie clear or the 302 redirect.
func TestLogout_Audit_WriteFailureStillRedirects(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	sessID := uuid.New()
	iamFake := &fakeIAM{}
	logger := &captureSplitLogger{writeSecurityErr: errors.New("pgx: deadlock")}

	r := tenantedRequest(t, http.MethodPost, "/logout", nil, &tenancy.Tenant{ID: tenantID})
	r.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: sessID.String()})
	rec := httptest.NewRecorder()

	handler.Logout(iamFake, handler.WithLogoutAudit(logger))(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302 (audit failure must not block logout)", rec.Code)
	}
	clearedFound := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessioncookie.NameTenant && c.MaxAge < 0 {
			clearedFound = true
		}
	}
	if !clearedFound {
		t.Errorf("tenant cookie clear not emitted on audit failure")
	}
}

// TestLogout_SetCookieAttributes_OnClear is the SIN-63188 AC #2
// equivalent for /logout: the Set-Cookie that drops the tenant
// session MUST mirror the original flags (Secure, HttpOnly,
// SameSite=Lax, Path=/) — the browser ignores cookie deletion that
// fails to repeat the issue-time flags.
func TestLogout_SetCookieAttributes_OnClear(t *testing.T) {
	t.Parallel()
	iamFake := &fakeIAM{}
	r := tenantedRequest(t, http.MethodPost, "/logout", nil, &tenancy.Tenant{ID: uuid.New()})
	rec := httptest.NewRecorder()
	handler.Logout(iamFake)(rec, r)

	// There may be multiple Set-Cookie headers; we only assert the
	// tenant cookie. Header().Values returns every value verbatim.
	wantPrefix := sessioncookie.NameTenant + "="
	var match string
	for _, h := range rec.Header().Values("Set-Cookie") {
		if strings.HasPrefix(h, wantPrefix) {
			match = h
			break
		}
	}
	if match == "" {
		t.Fatalf("Set-Cookie for %s missing", sessioncookie.NameTenant)
	}
	for _, want := range []string{
		"Path=/",
		"Max-Age=0", // negative MaxAge serialises as Max-Age=0
		"HttpOnly",
		"Secure",
		"SameSite=Lax",
	} {
		if !strings.Contains(match, want) {
			t.Errorf("Set-Cookie missing %q in: %q", want, match)
		}
	}
}
