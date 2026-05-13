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

func TestNewHTTPSession_PanicsOnNilStore(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic, got none")
		}
	}()
	mastermfa.NewHTTPSession(nil)
}

// ---------------------------------------------------------------------------
// IsVerified
// ---------------------------------------------------------------------------

func TestHTTPSession_IsVerified_NoCookieReturnsFalse(t *testing.T) {
	store := newFakeSessionStore()
	a := mastermfa.NewHTTPSession(store)
	r := httptest.NewRequest(http.MethodGet, "/m/x", nil)
	got, err := a.IsVerified(r)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got {
		t.Errorf("expected false (no cookie); got true")
	}
	if store.getCalls != 0 {
		t.Errorf("Get ran with no cookie: %d", store.getCalls)
	}
}

func TestHTTPSession_IsVerified_UnparseableCookieReturnsErr(t *testing.T) {
	store := newFakeSessionStore()
	a := mastermfa.NewHTTPSession(store)
	r := httptest.NewRequest(http.MethodGet, "/m/x", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: "not-a-uuid"})
	_, err := a.IsVerified(r)
	if err == nil {
		t.Fatal("expected err on unparseable cookie")
	}
	if !errors.Is(err, mastermfa.ErrSessionMFAState) {
		t.Errorf("err: got %v, want errors.Is ErrSessionMFAState", err)
	}
}

func TestHTTPSession_IsVerified_VerifiedSession(t *testing.T) {
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	if _, err := store.MarkVerified(context.Background(), sess.ID); err != nil {
		t.Fatal(err)
	}
	a := mastermfa.NewHTTPSession(store)
	r := httptest.NewRequest(http.MethodGet, "/m/x", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sess.ID.String()})
	got, err := a.IsVerified(r)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got {
		t.Errorf("expected true on a verified session")
	}
}

func TestHTTPSession_IsVerified_UnverifiedSession(t *testing.T) {
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	a := mastermfa.NewHTTPSession(store)
	r := httptest.NewRequest(http.MethodGet, "/m/x", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sess.ID.String()})
	got, err := a.IsVerified(r)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got {
		t.Errorf("expected false on an unverified session")
	}
}

func TestHTTPSession_IsVerified_NotFoundReturnsFalse(t *testing.T) {
	store := newFakeSessionStore() // no rows → ErrSessionNotFound
	a := mastermfa.NewHTTPSession(store)
	r := httptest.NewRequest(http.MethodGet, "/m/x", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: uuid.New().String()})
	got, err := a.IsVerified(r)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got {
		t.Errorf("expected false on a missing session")
	}
}

func TestHTTPSession_IsVerified_ExpiredReturnsFalse(t *testing.T) {
	store := newFakeSessionStore()
	store.getErr = mastermfa.ErrSessionExpired
	a := mastermfa.NewHTTPSession(store)
	r := httptest.NewRequest(http.MethodGet, "/m/x", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: uuid.New().String()})
	got, err := a.IsVerified(r)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got {
		t.Errorf("expected false on an expired session")
	}
}

func TestHTTPSession_IsVerified_TransientStorageErrorWraps(t *testing.T) {
	store := newFakeSessionStore()
	store.getErr = errors.New("pgx: i/o timeout")
	a := mastermfa.NewHTTPSession(store)
	r := httptest.NewRequest(http.MethodGet, "/m/x", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: uuid.New().String()})
	_, err := a.IsVerified(r)
	if err == nil {
		t.Fatal("expected err on transient storage failure")
	}
	if !errors.Is(err, mastermfa.ErrSessionMFAState) {
		t.Errorf("err did not wrap ErrSessionMFAState: %v", err)
	}
}

// ---------------------------------------------------------------------------
// MarkVerified
// ---------------------------------------------------------------------------

func TestHTTPSession_MarkVerified_NoCookieReturnsErr(t *testing.T) {
	store := newFakeSessionStore()
	a := mastermfa.NewHTTPSession(store)
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", nil)
	w := httptest.NewRecorder()
	err := a.MarkVerified(w, r)
	if err == nil {
		t.Fatal("expected err on missing cookie")
	}
	if !errors.Is(err, mastermfa.ErrSessionMFAState) {
		t.Errorf("err did not wrap ErrSessionMFAState: %v", err)
	}
	if store.markVerifiedCalls != 0 {
		t.Errorf("MarkVerified ran without a cookie: %d", store.markVerifiedCalls)
	}
}

func TestHTTPSession_MarkVerified_UnparseableCookieReturnsErr(t *testing.T) {
	store := newFakeSessionStore()
	a := mastermfa.NewHTTPSession(store)
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: "junk"})
	w := httptest.NewRecorder()
	err := a.MarkVerified(w, r)
	if err == nil {
		t.Fatal("expected err on unparseable cookie")
	}
	if !errors.Is(err, mastermfa.ErrSessionMFAState) {
		t.Errorf("err did not wrap ErrSessionMFAState: %v", err)
	}
}

func TestHTTPSession_MarkVerified_HappyPath(t *testing.T) {
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	a := mastermfa.NewHTTPSession(store)
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sess.ID.String()})
	w := httptest.NewRecorder()
	if err := a.MarkVerified(w, r); err != nil {
		t.Fatalf("err: %v", err)
	}
	if store.markVerifiedCalls != 1 {
		t.Errorf("MarkVerified calls: got %d want 1", store.markVerifiedCalls)
	}
	if store.lastMarkID != sess.ID {
		t.Errorf("MarkVerified id: got %v want %v", store.lastMarkID, sess.ID)
	}
}

func TestHTTPSession_MarkVerified_StoreError_Wraps(t *testing.T) {
	store := newFakeSessionStore()
	store.markVerifiedErr = errors.New("pgx: deadlock")
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	a := mastermfa.NewHTTPSession(store)
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sess.ID.String()})
	w := httptest.NewRecorder()
	err := a.MarkVerified(w, r)
	if err == nil {
		t.Fatal("expected err")
	}
	if !errors.Is(err, mastermfa.ErrSessionMFAState) {
		t.Errorf("err did not wrap ErrSessionMFAState: %v", err)
	}
}

// ---------------------------------------------------------------------------
// VerifiedAt
// ---------------------------------------------------------------------------

func TestHTTPSession_VerifiedAt_ReturnsZeroForUnverified(t *testing.T) {
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	a := mastermfa.NewHTTPSession(store)
	got, err := a.VerifiedAt(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("expected zero time for unverified session; got %v", got)
	}
}

func TestHTTPSession_VerifiedAt_ReturnsTimestampForVerified(t *testing.T) {
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	if _, err := store.MarkVerified(context.Background(), sess.ID); err != nil {
		t.Fatal(err)
	}
	a := mastermfa.NewHTTPSession(store)
	got, err := a.VerifiedAt(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.IsZero() {
		t.Errorf("expected non-zero time for verified session")
	}
}

func TestHTTPSession_VerifiedAt_PropagatesNotFound(t *testing.T) {
	store := newFakeSessionStore()
	a := mastermfa.NewHTTPSession(store)
	_, err := a.VerifiedAt(context.Background(), uuid.New())
	if !errors.Is(err, mastermfa.ErrSessionNotFound) {
		t.Errorf("err: got %v want ErrSessionNotFound", err)
	}
}

func TestHTTPSession_VerifiedAt_ExpiredCollapsesToNotFound(t *testing.T) {
	store := newFakeSessionStore()
	store.getErr = mastermfa.ErrSessionExpired
	a := mastermfa.NewHTTPSession(store)
	_, err := a.VerifiedAt(context.Background(), uuid.New())
	if !errors.Is(err, mastermfa.ErrSessionNotFound) {
		t.Errorf("err: got %v want ErrSessionNotFound", err)
	}
}

func TestHTTPSession_VerifiedAt_PropagatesTransientError(t *testing.T) {
	store := newFakeSessionStore()
	store.getErr = errors.New("pgx: ssl handshake failed")
	a := mastermfa.NewHTTPSession(store)
	_, err := a.VerifiedAt(context.Background(), uuid.New())
	if err == nil || errors.Is(err, mastermfa.ErrSessionNotFound) {
		t.Errorf("expected transient error, got %v", err)
	}
}
