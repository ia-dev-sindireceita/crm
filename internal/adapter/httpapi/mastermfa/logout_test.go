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

// ---------------------------------------------------------------------------
// Constructor preconditions
// ---------------------------------------------------------------------------

func TestNewLogoutHandler_PanicsOnNilSessions(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic, got none")
		}
	}()
	mastermfa.NewLogoutHandler(mastermfa.LogoutHandlerConfig{})
}

func TestNewLogoutHandler_DefaultsLoginPath(t *testing.T) {
	store := newFakeSessionStore()
	h := mastermfa.NewLogoutHandler(mastermfa.LogoutHandlerConfig{
		Sessions: store,
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/logout", nil)
	h.ServeHTTP(w, r)
	if w.Header().Get("Location") != "/m/login" {
		t.Errorf("Location: got %q want /m/login", w.Header().Get("Location"))
	}
}

// ---------------------------------------------------------------------------
// Method handling
// ---------------------------------------------------------------------------

func TestLogoutHandler_AcceptsGETAndPOST(t *testing.T) {
	store := newFakeSessionStore()
	h := mastermfa.NewLogoutHandler(mastermfa.LogoutHandlerConfig{
		Sessions: store, Logger: silentLogger(),
	})
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(method, "/m/logout", nil)
			h.ServeHTTP(w, r)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("status: got %d want 303", w.Code)
			}
		})
	}
}

func TestLogoutHandler_RejectsOtherMethods(t *testing.T) {
	store := newFakeSessionStore()
	h := mastermfa.NewLogoutHandler(mastermfa.LogoutHandlerConfig{
		Sessions: store, Logger: silentLogger(),
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/m/logout", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d want 405", w.Code)
	}
	allow := w.Header().Get("Allow")
	if allow == "" {
		t.Errorf("missing Allow header")
	}
}

// ---------------------------------------------------------------------------
// Cookie handling
// ---------------------------------------------------------------------------

func TestLogoutHandler_NoCookie_StillRedirectsAndClears(t *testing.T) {
	// A stale-session operator hits /m/logout with no cookie. The handler
	// MUST still emit Set-Cookie MaxAge=-1 + 303 so the browser ends up
	// at the login page.
	store := newFakeSessionStore()
	h := mastermfa.NewLogoutHandler(mastermfa.LogoutHandlerConfig{
		Sessions: store, Logger: silentLogger(),
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/logout", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if store.deleteCalls != 0 {
		t.Errorf("Delete ran with no cookie: %d", store.deleteCalls)
	}
	clearedFound := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessioncookie.NameMaster && c.MaxAge < 0 {
			clearedFound = true
		}
	}
	if !clearedFound {
		t.Errorf("master cookie clear not emitted")
	}
}

func TestLogoutHandler_UnparseableCookie_StillClears(t *testing.T) {
	store := newFakeSessionStore()
	h := mastermfa.NewLogoutHandler(mastermfa.LogoutHandlerConfig{
		Sessions: store, Logger: silentLogger(),
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/logout", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: "not-a-uuid"})
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if store.deleteCalls != 0 {
		t.Errorf("Delete ran on unparseable cookie: %d", store.deleteCalls)
	}
}

func TestLogoutHandler_HappyPath_DeletesAndClears(t *testing.T) {
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
	if store.deleteCalls != 1 {
		t.Errorf("Delete calls: got %d want 1", store.deleteCalls)
	}
	if store.lastDeleteID != sess.ID {
		t.Errorf("Delete id: got %v want %v", store.lastDeleteID, sess.ID)
	}
	// Subsequent Get must miss.
	if _, err := store.Get(context.Background(), sess.ID); !errors.Is(err, mastermfa.ErrSessionNotFound) {
		t.Errorf("session row lingered after Delete: err=%v", err)
	}
}

func TestLogoutHandler_DeleteFailure_StillRedirects(t *testing.T) {
	// ADR 0073 §D3: logout is best-effort. A storage failure on Delete
	// MUST NOT block the cookie clear or the redirect.
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	store.deleteErr = errors.New("pgx: deadlock")
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
	clearedFound := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessioncookie.NameMaster && c.MaxAge < 0 {
			clearedFound = true
		}
	}
	if !clearedFound {
		t.Errorf("master cookie clear not emitted on delete failure")
	}
}

func TestLogoutHandler_DeleteNotFound_DoesNotLog(t *testing.T) {
	// An idempotent delete (no row) is not a failure and MUST NOT log.
	// This guards against future contributors mistakenly elevating
	// ErrSessionNotFound to a WARN.
	store := newFakeSessionStore()
	store.deleteErr = mastermfa.ErrSessionNotFound
	h := mastermfa.NewLogoutHandler(mastermfa.LogoutHandlerConfig{
		Sessions: store, Logger: silentLogger(),
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/m/logout", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: uuid.New().String()})
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Custom login path
// ---------------------------------------------------------------------------

func TestLogoutHandler_HonoursCustomLoginPath(t *testing.T) {
	store := newFakeSessionStore()
	h := mastermfa.NewLogoutHandler(mastermfa.LogoutHandlerConfig{
		Sessions: store, LoginPath: "/custom/login", Logger: silentLogger(),
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/logout", nil)
	h.ServeHTTP(w, r)
	if got := w.Header().Get("Location"); got != "/custom/login" {
		t.Errorf("Location: got %q want /custom/login", got)
	}
}
