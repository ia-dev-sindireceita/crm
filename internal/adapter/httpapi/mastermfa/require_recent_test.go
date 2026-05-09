package mastermfa_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
)

// fakeRecentSessions scripts MasterSessionRecentMFA for the
// RequireRecentMFA tests. The verifiedAt field is the value returned
// to the middleware; err overrides it when non-nil.
type fakeRecentSessions struct {
	verifiedAt time.Time
	err        error
	calls      int
}

func (f *fakeRecentSessions) VerifiedAt(_ *http.Request) (time.Time, error) {
	f.calls++
	if f.err != nil {
		return time.Time{}, f.err
	}
	return f.verifiedAt, nil
}

func newRecentMW(sessions mastermfa.MasterSessionRecentMFA, maxAge time.Duration, now func() time.Time) func(http.Handler) http.Handler {
	return mastermfa.RequireRecentMFA(mastermfa.RequireRecentMFAConfig{
		Sessions: sessions,
		MaxAge:   maxAge,
		Now:      now,
	})
}

func reqWithMaster(target string) (*http.Request, uuid.UUID) {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	uid := uuid.New()
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uid, Email: "ops@example.com"}))
	return r, uid
}

func TestRequireRecentMFA_PanicsOnNilSessions(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil Sessions")
		}
	}()
	mastermfa.RequireRecentMFA(mastermfa.RequireRecentMFAConfig{MaxAge: time.Minute})
}

func TestRequireRecentMFA_PanicsOnZeroOrNegativeMaxAge(t *testing.T) {
	cases := []struct {
		name   string
		maxAge time.Duration
	}{
		{"zero", 0},
		{"negative", -time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for MaxAge=%v", tc.maxAge)
				}
			}()
			mastermfa.RequireRecentMFA(mastermfa.RequireRecentMFAConfig{
				Sessions: &fakeRecentSessions{},
				MaxAge:   tc.maxAge,
			})
		})
	}
}

func TestRequireRecentMFA_DefaultsApplied(t *testing.T) {
	// Build with empty VerifyPath/Logger/Now and assert the default
	// "/m/2fa/verify" target by triggering the not-verified branch.
	frozen := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)
	mw := newRecentMW(&fakeRecentSessions{verifiedAt: time.Time{}}, time.Minute, nil)
	next := &downstream{}
	h := mw(next)

	r, _ := reqWithMaster("/m/grant_courtesy")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	_ = frozen // silence unused variable warning — Now defaults to time.Now and is exercised on the freshness branch only

	if next.calls != 0 {
		t.Fatalf("downstream should not be reached; calls=%d", next.calls)
	}
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/m/2fa/verify") {
		t.Fatalf("location: got %q, want /m/2fa/verify prefix (default)", loc)
	}
}

func TestRequireRecentMFA_PassesWhenVerifiedWithinWindow(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	verified := now.Add(-5 * time.Minute) // fresh
	sessions := &fakeRecentSessions{verifiedAt: verified}
	next := &downstream{}
	h := newRecentMW(sessions, 15*time.Minute, func() time.Time { return now })(next)

	r, _ := reqWithMaster("/m/grant_courtesy")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if next.calls != 1 {
		t.Fatalf("downstream calls: got %d, want 1", next.calls)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if sessions.calls != 1 {
		t.Fatalf("VerifiedAt calls: got %d, want 1", sessions.calls)
	}
}

func TestRequireRecentMFA_PassesAtExactBoundary(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	verified := now.Add(-15 * time.Minute) // exactly at the boundary
	sessions := &fakeRecentSessions{verifiedAt: verified}
	next := &downstream{}
	h := newRecentMW(sessions, 15*time.Minute, func() time.Time { return now })(next)

	r, _ := reqWithMaster("/m/grant_courtesy")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	// `now - verified == MaxAge` is not stale (the comparison is `>`).
	if next.calls != 1 {
		t.Fatalf("downstream calls at boundary: got %d, want 1", next.calls)
	}
}

func TestRequireRecentMFA_RedirectsWhenStale(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	verified := now.Add(-15*time.Minute - time.Nanosecond) // one ns past the boundary
	sessions := &fakeRecentSessions{verifiedAt: verified}
	next := &downstream{}
	h := newRecentMW(sessions, 15*time.Minute, func() time.Time { return now })(next)

	r, _ := reqWithMaster("/m/grant_courtesy?x=y")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if next.calls != 0 {
		t.Fatalf("downstream calls: got %d, want 0", next.calls)
	}
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/m/2fa/verify?") {
		t.Fatalf("location: got %q, want /m/2fa/verify?... prefix", loc)
	}
	if !strings.Contains(loc, "return=") {
		t.Fatalf("location missing return= param: %q", loc)
	}
	if !strings.Contains(loc, "%2Fm%2Fgrant_courtesy") {
		t.Fatalf("location does not preserve original path: %q", loc)
	}
}

func TestRequireRecentMFA_RedirectsWhenZeroVerifiedAt(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	sessions := &fakeRecentSessions{verifiedAt: time.Time{}}
	next := &downstream{}
	h := newRecentMW(sessions, time.Minute, func() time.Time { return now })(next)

	r, _ := reqWithMaster("/m/grant_courtesy")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if next.calls != 0 {
		t.Fatal("downstream should not be reached on zero verified_at")
	}
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("Location"), "/m/2fa/verify") {
		t.Fatalf("location: got %q, want /m/2fa/verify prefix", w.Header().Get("Location"))
	}
}

func TestRequireRecentMFA_RedirectsWhenSessionNotFound(t *testing.T) {
	sessions := &fakeRecentSessions{err: mastermfa.ErrSessionNotFound}
	next := &downstream{}
	h := newRecentMW(sessions, time.Minute, time.Now)(next)

	r, _ := reqWithMaster("/m/grant_courtesy")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if next.calls != 0 {
		t.Fatal("downstream should not be reached when session is missing")
	}
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("Location"), "/m/2fa/verify") {
		t.Fatalf("location: got %q, want /m/2fa/verify prefix", w.Header().Get("Location"))
	}
}

func TestRequireRecentMFA_500OnTransientStorageError(t *testing.T) {
	sessions := &fakeRecentSessions{err: errors.New("redis down")}
	next := &downstream{}
	h := newRecentMW(sessions, time.Minute, time.Now)(next)

	r, _ := reqWithMaster("/m/grant_courtesy")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if next.calls != 0 {
		t.Fatal("downstream should not be reached on storage error")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", w.Code)
	}
}

func TestRequireRecentMFA_401WhenMasterMissingFromContext(t *testing.T) {
	sessions := &fakeRecentSessions{verifiedAt: time.Now()}
	next := &downstream{}
	h := newRecentMW(sessions, time.Minute, time.Now)(next)

	// Note: no WithMaster — explicitly bypassing master injection.
	r := httptest.NewRequest(http.MethodGet, "/m/grant_courtesy", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if next.calls != 0 {
		t.Fatal("downstream should not be reached without master in context")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", w.Code)
	}
	if sessions.calls != 0 {
		t.Fatalf("VerifiedAt should not be called when master missing; calls=%d", sessions.calls)
	}
}

func TestRequireRecentMFA_CustomVerifyPath(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	sessions := &fakeRecentSessions{verifiedAt: time.Time{}}
	next := &downstream{}
	mw := mastermfa.RequireRecentMFA(mastermfa.RequireRecentMFAConfig{
		Sessions:   sessions,
		MaxAge:     time.Minute,
		VerifyPath: "/m/auth/step-up",
		Now:        func() time.Time { return now },
	})
	h := mw(next)

	r, _ := reqWithMaster("/m/grant_courtesy")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if !strings.HasPrefix(w.Header().Get("Location"), "/m/auth/step-up") {
		t.Fatalf("location: got %q, want /m/auth/step-up prefix", w.Header().Get("Location"))
	}
}

func TestRequireRecentMFA_DropsUnsafeReturnPath(t *testing.T) {
	// Original URL "//evil/foo" is scheme-relative — isSafeReturnPath
	// rejects it and the redirect goes to the bare verify path.
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	sessions := &fakeRecentSessions{verifiedAt: time.Time{}}
	next := &downstream{}
	h := newRecentMW(sessions, time.Minute, func() time.Time { return now })(next)

	r := httptest.NewRequest(http.MethodGet, "http://example.com//evil/foo", nil)
	r.URL.Path = "//evil/foo"
	uid := uuid.New()
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uid, Email: "ops@example.com"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	loc := w.Header().Get("Location")
	if loc != "/m/2fa/verify" {
		t.Fatalf("location: got %q, want bare /m/2fa/verify (unsafe return dropped)", loc)
	}
}

func TestRequireRecentMFA_DoesNotConsumeBody(t *testing.T) {
	// The middleware must not read the request body — handlers downstream
	// rely on r.Body being intact (e.g. the recovery regenerate POST that
	// reads form fields after the gate). We exercise this by attaching a
	// body and asserting the downstream sees the same bytes.
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	sessions := &fakeRecentSessions{verifiedAt: now.Add(-time.Minute)}

	var got string
	bodyReader := func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	}
	mw := newRecentMW(sessions, time.Hour, func() time.Time { return now })
	h := mw(http.HandlerFunc(bodyReader))

	r := httptest.NewRequest(http.MethodPost, "/m/grant_courtesy", strings.NewReader("payload=ok"))
	uid := uuid.New()
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uid, Email: "ops@example.com"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if got != "payload=ok" {
		t.Fatalf("downstream body: got %q, want %q", got, "payload=ok")
	}
}
