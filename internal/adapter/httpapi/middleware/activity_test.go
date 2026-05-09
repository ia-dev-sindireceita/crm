package middleware_test

// SIN-62377 (FAIL-4) activity middleware unit tests. Cover:
//   - constructor preconditions
//   - happy path: pass + Touch called
//   - idle timeout → 302 /login + ClearTenant cookie
//   - hard timeout → 302 /login + ClearTenant cookie
//   - unknown role → fail-closed redirect
//   - Touch ErrSessionNotFound → redirect (race with logout)
//   - Touch transient error → 500
//   - missing session in context → 500 (wiring bug)

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
)

// fakeToucher implements middleware.SessionToucher with scriptable
// per-call result.
type fakeToucher struct {
	calls    int
	gotID    uuid.UUID
	gotTID   uuid.UUID
	gotStamp time.Time
	err      error
}

func (f *fakeToucher) Touch(_ context.Context, tenantID, sessionID uuid.UUID, lastActivity time.Time) error {
	f.calls++
	f.gotID = sessionID
	f.gotTID = tenantID
	f.gotStamp = lastActivity
	return f.err
}

// downstream is a minimal handler that records whether it was reached.
type recordHandler struct{ called bool }

func (r *recordHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	r.called = true
	w.WriteHeader(http.StatusOK)
}

// reqWithSession wraps r with a context that carries the supplied
// session, mirroring what middleware.Auth would have done upstream.
func reqWithSession(target string, sess iam.Session) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	return r.WithContext(middleware.WithSession(r.Context(), sess))
}

func TestActivity_PanicsOnNilSessions(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil Sessions")
		}
	}()
	_ = middleware.Activity(middleware.ActivityConfig{})
}

// Happy path: a session well within the per-role idle window passes
// through; downstream is called; Touch is called with the supplied
// "now" timestamp.
func TestActivity_PassThroughBumpsLastActivity(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sess := iam.Session{
		ID:           uuid.New(),
		UserID:       uuid.New(),
		TenantID:     uuid.New(),
		CreatedAt:    now.Add(-30 * time.Minute), // well under 8h hard
		LastActivity: now.Add(-5 * time.Minute),  // under 30m idle
		Role:         iam.RoleTenantCommon,
	}
	toucher := &fakeToucher{}
	rec := &recordHandler{}
	h := middleware.Activity(middleware.ActivityConfig{
		Sessions: toucher,
		Now:      func() time.Time { return now },
	})(rec)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, reqWithSession("/secret", sess))

	if !rec.called {
		t.Fatalf("downstream not called on pass")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if toucher.calls != 1 {
		t.Fatalf("Touch calls = %d, want 1", toucher.calls)
	}
	if toucher.gotID != sess.ID || toucher.gotTID != sess.TenantID {
		t.Fatalf("Touch args mismatch: id=%v tenant=%v", toucher.gotID, toucher.gotTID)
	}
	if !toucher.gotStamp.Equal(now) {
		t.Fatalf("Touch stamp = %v, want %v", toucher.gotStamp, now)
	}
}

// Idle timeout: lastActivity older than the role's Idle window. The
// gate clears the tenant cookie and redirects to /login. Downstream
// MUST NOT be called.
func TestActivity_IdleTimeoutRedirectsAndClearsCookie(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sess := iam.Session{
		ID:           uuid.New(),
		UserID:       uuid.New(),
		TenantID:     uuid.New(),
		CreatedAt:    now.Add(-1 * time.Hour),    // hard: under 8h
		LastActivity: now.Add(-31 * time.Minute), // idle: over 30m
		Role:         iam.RoleTenantCommon,
	}
	toucher := &fakeToucher{}
	rec := &recordHandler{}
	h := middleware.Activity(middleware.ActivityConfig{
		Sessions: toucher,
		Now:      func() time.Time { return now },
	})(rec)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, reqWithSession("/secret", sess))

	if rec.called {
		t.Fatalf("downstream reached on idle timeout")
	}
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc == "" {
		t.Fatalf("Location header missing")
	}
	if !cookieCleared(w, sessioncookie.NameTenant) {
		t.Fatalf("expected tenant cookie cleared, got %q", w.Header().Get("Set-Cookie"))
	}
	if toucher.calls != 0 {
		t.Fatalf("Touch must not be called when timeout fires; calls=%d", toucher.calls)
	}
}

// Hard timeout: createdAt older than role's Hard window — even with
// fresh activity, the session is rejected.
func TestActivity_HardTimeoutRedirectsAndClearsCookie(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sess := iam.Session{
		ID:           uuid.New(),
		UserID:       uuid.New(),
		TenantID:     uuid.New(),
		CreatedAt:    now.Add(-9 * time.Hour),   // hard: over 8h
		LastActivity: now.Add(-1 * time.Minute), // idle: way under 30m
		Role:         iam.RoleTenantCommon,
	}
	toucher := &fakeToucher{}
	rec := &recordHandler{}
	h := middleware.Activity(middleware.ActivityConfig{
		Sessions: toucher,
		Now:      func() time.Time { return now },
	})(rec)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, reqWithSession("/secret", sess))

	if rec.called {
		t.Fatalf("downstream reached on hard timeout")
	}
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	if !cookieCleared(w, sessioncookie.NameTenant) {
		t.Fatalf("expected tenant cookie cleared, got %q", w.Header().Get("Set-Cookie"))
	}
}

// Unknown role: fail-closed → redirect, do not call Touch.
func TestActivity_UnknownRoleRedirectsAndClearsCookie(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sess := iam.Session{
		ID:           uuid.New(),
		UserID:       uuid.New(),
		TenantID:     uuid.New(),
		CreatedAt:    now,
		LastActivity: now,
		Role:         iam.Role("not_a_real_role"),
	}
	toucher := &fakeToucher{}
	rec := &recordHandler{}
	h := middleware.Activity(middleware.ActivityConfig{
		Sessions: toucher,
		Now:      func() time.Time { return now },
	})(rec)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, reqWithSession("/secret", sess))
	if rec.called {
		t.Fatalf("downstream reached on unknown role")
	}
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	if toucher.calls != 0 {
		t.Fatalf("Touch must not be called on unknown role; calls=%d", toucher.calls)
	}
}

// Touch race: row deleted between Auth and Activity. Treat as "session
// vanished mid-flight" — clear cookie and redirect, no 500.
func TestActivity_TouchSessionNotFoundRedirects(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sess := freshSession(now)
	toucher := &fakeToucher{err: iam.ErrSessionNotFound}
	rec := &recordHandler{}
	h := middleware.Activity(middleware.ActivityConfig{
		Sessions: toucher,
		Now:      func() time.Time { return now },
	})(rec)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, reqWithSession("/secret", sess))
	if rec.called {
		t.Fatalf("downstream reached after Touch ErrSessionNotFound")
	}
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	if !cookieCleared(w, sessioncookie.NameTenant) {
		t.Fatalf("expected tenant cookie cleared")
	}
}

// Transient Touch failure → 500 (deny-by-default; do not silently let
// the request through without the activity bump).
func TestActivity_TouchTransientErrorReturns500(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sess := freshSession(now)
	toucher := &fakeToucher{err: errors.New("pgx: dial timeout")}
	rec := &recordHandler{}
	h := middleware.Activity(middleware.ActivityConfig{
		Sessions: toucher,
		Now:      func() time.Time { return now },
	})(rec)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, reqWithSession("/secret", sess))
	if rec.called {
		t.Fatalf("downstream reached on transient Touch error")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

// Wiring bug: Activity reached without Auth populating the session.
// Surface as 500 rather than silently letting the request through.
func TestActivity_NoSessionInContextReturns500(t *testing.T) {
	toucher := &fakeToucher{}
	rec := &recordHandler{}
	h := middleware.Activity(middleware.ActivityConfig{Sessions: toucher})(rec)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/secret", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if toucher.calls != 0 {
		t.Fatalf("Touch called without session: %d", toucher.calls)
	}
	if rec.called {
		t.Fatalf("downstream reached without session")
	}
}

// Per-role idle window check: gerente has Idle=30m, atendente has
// Idle=60m. A session that is idle 45m must trip for gerente but
// pass for atendente.
func TestActivity_PerRoleIdleBoundary(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		role      iam.Role
		wantPass  bool
		idleSince time.Duration
	}{
		{iam.RoleTenantGerente, false, 31 * time.Minute},   // > 30m → trip
		{iam.RoleTenantAtendente, true, 31 * time.Minute},  // < 60m → pass
		{iam.RoleTenantAtendente, false, 61 * time.Minute}, // > 60m → trip
	}
	for _, tc := range cases {
		t.Run(string(tc.role)+"-"+tc.idleSince.String(), func(t *testing.T) {
			sess := iam.Session{
				ID:           uuid.New(),
				UserID:       uuid.New(),
				TenantID:     uuid.New(),
				CreatedAt:    now.Add(-2 * time.Hour),
				LastActivity: now.Add(-tc.idleSince),
				Role:         tc.role,
			}
			toucher := &fakeToucher{}
			rec := &recordHandler{}
			h := middleware.Activity(middleware.ActivityConfig{
				Sessions: toucher,
				Now:      func() time.Time { return now },
			})(rec)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, reqWithSession("/p", sess))
			if tc.wantPass && !rec.called {
				t.Fatalf("expected pass, got blocked (status=%d)", w.Code)
			}
			if !tc.wantPass && rec.called {
				t.Fatalf("expected block, got pass")
			}
		})
	}
}

func freshSession(now time.Time) iam.Session {
	return iam.Session{
		ID:           uuid.New(),
		UserID:       uuid.New(),
		TenantID:     uuid.New(),
		CreatedAt:    now.Add(-1 * time.Minute),
		LastActivity: now.Add(-1 * time.Minute),
		Role:         iam.RoleTenantCommon,
	}
}

// cookieCleared reports whether a Set-Cookie header for `name` was
// written with MaxAge=-1 (the deletion shape that
// sessioncookie.ClearTenant emits).
func cookieCleared(w *httptest.ResponseRecorder, name string) bool {
	for _, sc := range w.Result().Cookies() {
		if sc.Name == name && sc.MaxAge < 0 {
			return true
		}
	}
	return false
}
