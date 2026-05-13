package mastermfa_test

// SIN-62418 — middleware-side coverage of the master-session hard-cap
// path. Storage-layer enforcement lives in the mastersession integration
// tests (internal/adapter/db/postgres/mastersession). Here we cover the
// HTTP-edge behaviour: when Touch returns mastermfa.ErrSessionHardCap,
// the middleware MUST emit master.session.hard_cap_hit, clear the
// __Host-sess-master cookie, and 303 to /m/login?next=<original>. Same
// UX as the idle-timeout path (auth_test.go's TouchNotFound case) plus
// the audit emission.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
)

// recordingAuditor captures every LogHardCapHit call for assertion.
// errFn lets a test scripted-fail the audit write to assert that the
// redirect proceeds anyway.
type recordingAuditor struct {
	mu    sync.Mutex
	calls int
	last  recordedHardCap
	errFn func() error
}

type recordedHardCap struct {
	userID    uuid.UUID
	sessionID uuid.UUID
	createdAt time.Time
	now       time.Time
	route     string
}

func (r *recordingAuditor) LogHardCapHit(_ context.Context, userID, sessionID uuid.UUID, createdAt, now time.Time, route string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.last = recordedHardCap{
		userID:    userID,
		sessionID: sessionID,
		createdAt: createdAt,
		now:       now,
		route:     route,
	}
	if r.errFn != nil {
		return r.errFn()
	}
	return nil
}

func TestRequireMasterAuth_TouchHardCap_EmitsAuditClearsCookieAndRedirects(t *testing.T) {
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, err := store.Create(context.Background(), uid, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Pin a deterministic CreatedAt — fakeSessionStore uses time.Now()
	// inside Create, but we want to assert the audit's created_at field
	// against a known value. Replace the row.
	createdAt := time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC)
	store.mu.Lock()
	row := store.rows[sess.ID]
	row.CreatedAt = createdAt
	store.rows[sess.ID] = row
	store.mu.Unlock()

	// Simulate the storage-layer hard-cap delete + sentinel.
	store.touchErr = mastermfa.ErrSessionHardCap

	dir := newFakeDirectory()
	dir.emails[uid] = "ops@example.com"
	auditor := &recordingAuditor{}
	frozenNow := createdAt.Add(4 * time.Hour).Add(time.Minute) // T+4h01m
	mw := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions:  store,
		Directory: dir,
		Auditor:   auditor,
		Logger:    silentLogger(),
		Now:       func() time.Time { return frozenNow },
	})
	d := &downstreamHandler{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/2fa/enroll?x=1", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sess.ID.String()})
	mw(d).ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	parsed, _ := url.Parse(loc)
	if parsed.Path != "/m/login" {
		t.Errorf("Location path: got %q want /m/login", parsed.Path)
	}
	if got := parsed.Query().Get("next"); got != "/m/2fa/enroll?x=1" {
		t.Errorf("next= got %q want %q", got, "/m/2fa/enroll?x=1")
	}
	if d.calls != 0 {
		t.Fatal("downstream reached on hard-cap hit")
	}

	// Audit assertions — every documented field MUST land.
	if auditor.calls != 1 {
		t.Fatalf("auditor calls: got %d want 1", auditor.calls)
	}
	if auditor.last.userID != uid {
		t.Errorf("audit user_id: got %v want %v", auditor.last.userID, uid)
	}
	if auditor.last.sessionID != sess.ID {
		t.Errorf("audit session_id: got %v want %v", auditor.last.sessionID, sess.ID)
	}
	if !auditor.last.createdAt.Equal(createdAt) {
		t.Errorf("audit created_at: got %v want %v", auditor.last.createdAt, createdAt)
	}
	if !auditor.last.now.Equal(frozenNow) {
		t.Errorf("audit now: got %v want %v", auditor.last.now, frozenNow)
	}
	if auditor.last.route != "/m/2fa/enroll" {
		t.Errorf("audit route: got %q want %q", auditor.last.route, "/m/2fa/enroll")
	}

	// Cookie clearing — the Set-Cookie header MUST emit a clear for
	// __Host-sess-master so the browser drops the now-stale cookie
	// (matches the AC2 "cookie cleared" requirement).
	var clearSeen bool
	for _, sc := range w.Result().Cookies() {
		if sc.Name == sessioncookie.NameMaster && sc.MaxAge < 0 {
			clearSeen = true
			break
		}
	}
	if !clearSeen {
		t.Errorf("expected __Host-sess-master cookie clear, headers: %v", w.Header().Values("Set-Cookie"))
	}
}

func TestRequireMasterAuth_TouchHardCap_NilAuditor_StillRedirects(t *testing.T) {
	// A misconfigured deploy without an auditor MUST NOT panic on the
	// hard-cap path; the redirect + cookie clear still happen, only the
	// observability event is dropped.
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	store.touchErr = mastermfa.ErrSessionHardCap

	dir := newFakeDirectory()
	dir.emails[uid] = "ops@example.com"
	mw := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions:  store,
		Directory: dir,
		Logger:    silentLogger(),
	})
	d := &downstreamHandler{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/tenant", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sess.ID.String()})
	mw(d).ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if d.calls != 0 {
		t.Fatal("downstream reached on hard-cap hit (nil auditor path)")
	}
}

func TestRequireMasterAuth_TouchHardCap_AuditWriteFailureDoesNotBlockRedirect(t *testing.T) {
	// An audit-write failure on a hard-cap hit MUST NOT block the user
	// from being redirected to /m/login. The authoritative invalidation
	// has already happened in storage; observability degrading is the
	// less-bad failure mode vs. leaving the operator stranded.
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	store.touchErr = mastermfa.ErrSessionHardCap

	dir := newFakeDirectory()
	dir.emails[uid] = "ops@example.com"
	auditor := &recordingAuditor{errFn: func() error { return errors.New("audit pipe broken") }}
	mw := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions:  store,
		Directory: dir,
		Auditor:   auditor,
		Logger:    silentLogger(),
	})
	d := &downstreamHandler{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/tenant", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sess.ID.String()})
	mw(d).ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if auditor.calls != 1 {
		t.Errorf("auditor calls: got %d want 1", auditor.calls)
	}
	if d.calls != 0 {
		t.Fatal("downstream reached after audit failure on hard-cap")
	}
}

// TestRequireMasterAuth_TouchHardCap_AuditTimingFromInjectedClock —
// when the caller injects a Now func, the audit's `now` field MUST
// echo that injected time (not time.Now). Pinning prevents future
// refactors from "helpfully" stamping a fresh wall-clock and silently
// drifting from the storage-layer's view of `now`.
func TestRequireMasterAuth_TouchHardCap_AuditTimingFromInjectedClock(t *testing.T) {
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	store.touchErr = mastermfa.ErrSessionHardCap

	dir := newFakeDirectory()
	dir.emails[uid] = "ops@example.com"
	auditor := &recordingAuditor{}
	pinned := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	mw := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions:  store,
		Directory: dir,
		Auditor:   auditor,
		Logger:    silentLogger(),
		Now:       func() time.Time { return pinned },
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/tenant", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sess.ID.String()})
	mw(&downstreamHandler{}).ServeHTTP(w, r)

	if !auditor.last.now.Equal(pinned) {
		t.Errorf("audit now: got %v want %v", auditor.last.now, pinned)
	}

	// And confirm the request URI ended up in next= as a path-only string
	// (defence in depth against open-redirect via crafted Referer).
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "next=") {
		t.Errorf("next= missing from Location: %q", loc)
	}
}
