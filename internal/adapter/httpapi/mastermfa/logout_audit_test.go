package mastermfa_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam/audit"
)

// captureSplitLogger is an in-memory audit.SplitLogger that records
// every Write call so the SIN-63188 logout-audit tests can assert the
// produced row shape. Errors can be scripted via writeSecurityErr.
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

// TestLogoutHandler_Audit_NotEmittedWithoutLogger guards the
// backward-compatible default: existing tests construct LogoutHandlerConfig
// without an AuditLogger and the handler MUST behave exactly as before
// (no panic, no audit, normal redirect).
func TestLogoutHandler_Audit_NotEmittedWithoutLogger(t *testing.T) {
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	h := mastermfa.NewLogoutHandler(mastermfa.LogoutHandlerConfig{
		Sessions: store, Logger: silentLogger(),
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/m/logout", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sess.ID.String()})
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
}

// TestLogoutHandler_Audit_EmitsLogoutRow asserts the SIN-63188 happy
// path: a successful master logout with an AuditLogger wired appends
// exactly one SecurityEventLogout row carrying the session id, the
// audience marker, the reason, and the operator user id.
func TestLogoutHandler_Audit_EmitsLogoutRow(t *testing.T) {
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	logger := &captureSplitLogger{}
	h := mastermfa.NewLogoutHandler(mastermfa.LogoutHandlerConfig{
		Sessions:    store,
		AuditLogger: logger,
		Logger:      silentLogger(),
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/m/logout", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sess.ID.String()})
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if got := len(logger.security); got != 1 {
		t.Fatalf("audit rows: got %d want 1", got)
	}
	row := logger.security[0]
	if row.Event != audit.SecurityEventLogout {
		t.Errorf("event: got %q want %q", row.Event, audit.SecurityEventLogout)
	}
	if row.ActorUserID != uid {
		t.Errorf("actor_user_id: got %v want %v", row.ActorUserID, uid)
	}
	if row.TenantID != nil {
		t.Errorf("tenant_id: got %v want nil (master is not tenant-scoped)", *row.TenantID)
	}
	if got := row.Target["session_id"]; got != sess.ID.String() {
		t.Errorf("target.session_id: got %v want %v", got, sess.ID.String())
	}
	if got := row.Target["audience"]; got != "master" {
		t.Errorf("target.audience: got %v want master", got)
	}
	if got := row.Target["reason"]; got != "user_initiated" {
		t.Errorf("target.reason: got %v want user_initiated", got)
	}
}

// TestLogoutHandler_Audit_NoSessionNoAudit asserts the "stale operator
// hits /m/logout with no cookie" branch does NOT emit an audit row —
// there is no principal to attribute the event to, and the cookie
// clear has already restored the desired post-condition.
func TestLogoutHandler_Audit_NoSessionNoAudit(t *testing.T) {
	store := newFakeSessionStore()
	logger := &captureSplitLogger{}
	h := mastermfa.NewLogoutHandler(mastermfa.LogoutHandlerConfig{
		Sessions:    store,
		AuditLogger: logger,
		Logger:      silentLogger(),
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/logout", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if len(logger.security) != 0 {
		t.Errorf("audit rows emitted with no session: %d", len(logger.security))
	}
}

// TestLogoutHandler_Audit_MissingRowNoAudit asserts the "cookie present
// but session row already gone" branch: Get returns ErrSessionNotFound,
// Delete is a no-op, and no audit row is emitted (we have no operator
// id to attribute the event to).
func TestLogoutHandler_Audit_MissingRowNoAudit(t *testing.T) {
	store := newFakeSessionStore()
	logger := &captureSplitLogger{}
	h := mastermfa.NewLogoutHandler(mastermfa.LogoutHandlerConfig{
		Sessions:    store,
		AuditLogger: logger,
		Logger:      silentLogger(),
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/logout", nil)
	// A cookie that parses as a uuid but corresponds to no row.
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: uuid.New().String()})
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if len(logger.security) != 0 {
		t.Errorf("audit rows emitted for missing session: %d", len(logger.security))
	}
}

// TestLogoutHandler_Audit_WriteFailureStillRedirects guards the
// "audit best-effort" invariant from ADR 0073 §D3 / ADR 0102: a
// SplitLogger that returns an error MUST NOT block the cookie clear
// or the 303 redirect.
func TestLogoutHandler_Audit_WriteFailureStillRedirects(t *testing.T) {
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	logger := &captureSplitLogger{writeSecurityErr: errors.New("pgx: deadlock")}
	h := mastermfa.NewLogoutHandler(mastermfa.LogoutHandlerConfig{
		Sessions:    store,
		AuditLogger: logger,
		Logger:      silentLogger(),
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/m/logout", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sess.ID.String()})
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303 (audit failure must not block)", w.Code)
	}
	clearedFound := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessioncookie.NameMaster && c.MaxAge < 0 {
			clearedFound = true
		}
	}
	if !clearedFound {
		t.Errorf("master cookie clear not emitted on audit failure")
	}
}

// TestLogoutHandler_SetCookieAttributes_OnClear is the SIN-63188 AC #2
// equivalent for /m/logout: the Set-Cookie that drops the master
// session MUST mirror the original flags (Secure, HttpOnly, SameSite=
// Strict, Path=/) — without that the browser silently ignores the
// deletion. Holds even when no session was in flight.
func TestLogoutHandler_SetCookieAttributes_OnClear(t *testing.T) {
	store := newFakeSessionStore()
	h := mastermfa.NewLogoutHandler(mastermfa.LogoutHandlerConfig{
		Sessions: store, Logger: silentLogger(),
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/logout", nil)
	h.ServeHTTP(w, r)

	header := w.Header().Get("Set-Cookie")
	if header == "" {
		t.Fatalf("Set-Cookie missing on /m/logout response")
	}
	for _, want := range []string{
		sessioncookie.NameMaster + "=",
		"Path=/",
		"Max-Age=0", // negative MaxAge serialises as Max-Age=0
		"HttpOnly",
		"Secure",
		"SameSite=Strict",
	} {
		if !strings.Contains(header, want) {
			t.Errorf("Set-Cookie missing %q in: %q", want, header)
		}
	}
}
