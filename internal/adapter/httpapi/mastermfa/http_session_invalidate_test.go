package mastermfa_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
)

// SIN-62380 (CAVEAT-3): HTTPSession.Invalidate satisfies
// MasterSessionInvalidator. The verify handler calls it on the
// 5-strike lockout trip to delete the master_session row and clear
// the __Host-sess-master cookie.

func TestHTTPSession_Invalidate_HappyPath(t *testing.T) {
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	a := mastermfa.NewHTTPSession(store)

	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sess.ID.String()})
	w := httptest.NewRecorder()

	if err := a.Invalidate(w, r); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if store.deleteCalls != 1 {
		t.Errorf("Delete calls: got %d want 1", store.deleteCalls)
	}
	if store.lastDeleteID != sess.ID {
		t.Errorf("Delete id: got %v want %v", store.lastDeleteID, sess.ID)
	}
	// Cookie was cleared (Max-Age < 0).
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessioncookie.NameMaster && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("master cookie was not cleared")
	}
}

func TestHTTPSession_Invalidate_NoCookieIsNoop(t *testing.T) {
	// A missing cookie still clears the response cookie (defensive
	// scrub) and returns nil — the post-condition (no live row, no
	// cookie) is satisfied either way.
	store := newFakeSessionStore()
	a := mastermfa.NewHTTPSession(store)

	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", nil)
	w := httptest.NewRecorder()

	if err := a.Invalidate(w, r); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if store.deleteCalls != 0 {
		t.Errorf("Delete ran without a cookie: %d", store.deleteCalls)
	}
}

func TestHTTPSession_Invalidate_UnparseableCookieIsNoop(t *testing.T) {
	// Junk cookie value: nothing to delete server-side, but the
	// cookie is still scrubbed.
	store := newFakeSessionStore()
	a := mastermfa.NewHTTPSession(store)

	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: "not-a-uuid"})
	w := httptest.NewRecorder()

	if err := a.Invalidate(w, r); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if store.deleteCalls != 0 {
		t.Errorf("Delete ran on unparseable cookie: %d", store.deleteCalls)
	}
}

func TestHTTPSession_Invalidate_StoreErrorWraps(t *testing.T) {
	// A storage failure on Delete is wrapped with ErrSessionMFAState
	// so the verify handler can errors.Is on the canonical sentinel.
	// The cookie is cleared regardless — leaving the user with a
	// cookie pointing at a row that may or may not exist would be
	// worse than denying them.
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	store.deleteErr = errors.New("pgx: deadlock")
	a := mastermfa.NewHTTPSession(store)

	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sess.ID.String()})
	w := httptest.NewRecorder()

	err := a.Invalidate(w, r)
	if err == nil {
		t.Fatal("expected error on Delete failure")
	}
	if !errors.Is(err, mastermfa.ErrSessionMFAState) {
		t.Errorf("err did not wrap ErrSessionMFAState: %v", err)
	}
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessioncookie.NameMaster && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("cookie was not scrubbed despite Delete failure")
	}
}

// Compile-time assertion that HTTPSession satisfies the new port. A
// runtime check would be nicer but the var-as-test is the idiomatic
// Go shape for an interface guarantee.
func TestHTTPSession_SatisfiesMasterSessionInvalidator(t *testing.T) {
	store := newFakeSessionStore()
	var _ mastermfa.MasterSessionInvalidator = mastermfa.NewHTTPSession(store)
}
