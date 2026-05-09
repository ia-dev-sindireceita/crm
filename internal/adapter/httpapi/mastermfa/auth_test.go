package mastermfa_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

// fakeSessionStore is a hand-written in-memory implementation of
// mastermfa.SessionStore with scriptable per-method overrides. Tests
// use it across the auth-middleware, login-handler, http-session
// adapter, and logout-handler suites — one fake, one place to look
// when behaviour drifts.
type fakeSessionStore struct {
	mu sync.Mutex

	// Persistent storage of created rows by id.
	rows map[uuid.UUID]mastermfa.Session

	// Scripted error injection.
	createErr       error
	getErr          error
	deleteErr       error
	markVerifiedErr error
	touchErr        error

	// Call counters so tests can assert idempotence / wiring.
	createCalls       int
	getCalls          int
	deleteCalls       int
	markVerifiedCalls int
	touchCalls        int

	// Last-call recorders (most-recent-write semantics).
	lastCreateUserID uuid.UUID
	lastCreateTTL    time.Duration
	lastTouchID      uuid.UUID
	lastTouchTTL     time.Duration
	lastDeleteID     uuid.UUID
	lastMarkID       uuid.UUID
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{rows: make(map[uuid.UUID]mastermfa.Session)}
}

func (f *fakeSessionStore) Create(_ context.Context, userID uuid.UUID, ttl time.Duration) (mastermfa.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.lastCreateUserID = userID
	f.lastCreateTTL = ttl
	if f.createErr != nil {
		return mastermfa.Session{}, f.createErr
	}
	id := uuid.New()
	now := time.Now().UTC()
	row := mastermfa.Session{
		ID:        id,
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	f.rows[id] = row
	return row, nil
}

func (f *fakeSessionStore) Get(_ context.Context, sessionID uuid.UUID) (mastermfa.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return mastermfa.Session{}, f.getErr
	}
	row, ok := f.rows[sessionID]
	if !ok {
		return mastermfa.Session{}, mastermfa.ErrSessionNotFound
	}
	if !row.ExpiresAt.IsZero() && time.Now().After(row.ExpiresAt) {
		return mastermfa.Session{}, mastermfa.ErrSessionExpired
	}
	return row, nil
}

func (f *fakeSessionStore) Delete(_ context.Context, sessionID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	f.lastDeleteID = sessionID
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.rows, sessionID)
	return nil
}

func (f *fakeSessionStore) MarkVerified(_ context.Context, sessionID uuid.UUID) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markVerifiedCalls++
	f.lastMarkID = sessionID
	if f.markVerifiedErr != nil {
		return time.Time{}, f.markVerifiedErr
	}
	row, ok := f.rows[sessionID]
	if !ok {
		return time.Time{}, mastermfa.ErrSessionNotFound
	}
	now := time.Now().UTC()
	row.MFAVerifiedAt = &now
	f.rows[sessionID] = row
	return now, nil
}

func (f *fakeSessionStore) Touch(_ context.Context, sessionID uuid.UUID, idleTTL time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touchCalls++
	f.lastTouchID = sessionID
	f.lastTouchTTL = idleTTL
	if f.touchErr != nil {
		return f.touchErr
	}
	row, ok := f.rows[sessionID]
	if !ok {
		return mastermfa.ErrSessionNotFound
	}
	row.ExpiresAt = time.Now().Add(idleTTL)
	f.rows[sessionID] = row
	return nil
}

// RotateID swaps the row keyed by oldID for a new uuid.New() id,
// preserving every other field. Added in SIN-62377 to satisfy the
// mastermfa.SessionStore interface; auth-test cases that don't
// exercise rotation never reach this method.
func (f *fakeSessionStore) RotateID(_ context.Context, oldID uuid.UUID) (mastermfa.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[oldID]
	if !ok {
		return mastermfa.Session{}, mastermfa.ErrSessionNotFound
	}
	newID := uuid.New()
	row.ID = newID
	f.rows[newID] = row
	delete(f.rows, oldID)
	return row, nil
}

// fakeDirectory implements mastermfa.MasterUserDirectory with scriptable
// per-call email + error.
type fakeDirectory struct {
	emails  map[uuid.UUID]string
	err     error
	calls   int
	lastUID uuid.UUID
}

func newFakeDirectory() *fakeDirectory {
	return &fakeDirectory{emails: make(map[uuid.UUID]string)}
}

func (f *fakeDirectory) EmailFor(_ context.Context, userID uuid.UUID) (string, error) {
	f.calls++
	f.lastUID = userID
	if f.err != nil {
		return "", f.err
	}
	if e, ok := f.emails[userID]; ok {
		return e, nil
	}
	return "", nil
}

// silentLogger discards every log line so test output stays clean.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// downstreamHandler captures the most-recent ctx + request path so
// tests can assert that the master-auth middleware injected the
// expected Master into the context before delegating.
type downstreamHandler struct {
	calls       int
	lastMaster  mastermfa.Master
	lastPath    string
	lastHadCtx  bool
	contextWrap func(context.Context)
}

func (d *downstreamHandler) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	d.calls++
	d.lastPath = r.URL.Path
	if m, ok := mastermfa.MasterFromContext(r.Context()); ok {
		d.lastHadCtx = true
		d.lastMaster = m
	}
	if d.contextWrap != nil {
		d.contextWrap(r.Context())
	}
}

// ---------------------------------------------------------------------------
// Constructor preconditions
// ---------------------------------------------------------------------------

func TestRequireMasterAuth_PanicsOnMissingDeps(t *testing.T) {
	cases := map[string]mastermfa.RequireMasterAuthConfig{
		"nil sessions":  {Directory: newFakeDirectory()},
		"nil directory": {Sessions: newFakeSessionStore()},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic, got none")
				}
			}()
			mastermfa.RequireMasterAuth(cfg)
		})
	}
}

func TestRequireMasterAuth_DefaultsAreApplied(t *testing.T) {
	store := newFakeSessionStore()
	dir := newFakeDirectory()
	// Build the middleware with no LoginPath / IdleTTL / Logger overrides.
	mw := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions:  store,
		Directory: dir,
	})
	d := &downstreamHandler{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/tenant", nil) // no cookie
	mw(d).ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("Location"), "/m/login") {
		t.Errorf("Location did not default to /m/login: %q", w.Header().Get("Location"))
	}
}

// ---------------------------------------------------------------------------
// Cookie / parse failures
// ---------------------------------------------------------------------------

func TestRequireMasterAuth_NoCookie_RedirectsToLogin(t *testing.T) {
	store := newFakeSessionStore()
	dir := newFakeDirectory()
	mw := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions: store, Directory: dir, Logger: silentLogger(),
	})
	d := &downstreamHandler{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/tenant?x=1", nil)
	mw(d).ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	parsed, _ := url.Parse(loc)
	if parsed.Path != "/m/login" {
		t.Errorf("redirect path: got %q want /m/login", parsed.Path)
	}
	if got := parsed.Query().Get("next"); got != "/m/tenant?x=1" {
		t.Errorf("next param: got %q want /m/tenant?x=1", got)
	}
	if d.calls != 0 {
		t.Fatal("downstream reached without a cookie")
	}
	if store.getCalls != 0 {
		t.Errorf("session lookup ran with no cookie: %d", store.getCalls)
	}
}

func TestRequireMasterAuth_UnparseableCookie_RedirectsToLogin(t *testing.T) {
	store := newFakeSessionStore()
	dir := newFakeDirectory()
	mw := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions: store, Directory: dir, Logger: silentLogger(),
	})
	d := &downstreamHandler{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/tenant", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: "not-a-uuid"})
	mw(d).ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("Location"), "/m/login") {
		t.Errorf("Location: got %q", w.Header().Get("Location"))
	}
	if d.calls != 0 {
		t.Fatal("downstream reached on unparseable cookie")
	}
	if store.getCalls != 0 {
		t.Errorf("session store hit on parse failure: %d", store.getCalls)
	}
}

// ---------------------------------------------------------------------------
// Session-store outcomes
// ---------------------------------------------------------------------------

func TestRequireMasterAuth_SessionNotFound_RedirectsToLogin(t *testing.T) {
	store := newFakeSessionStore() // empty rows → Get returns ErrSessionNotFound
	dir := newFakeDirectory()
	mw := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions: store, Directory: dir, Logger: silentLogger(),
	})
	d := &downstreamHandler{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/tenant", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: uuid.New().String()})
	mw(d).ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if d.calls != 0 {
		t.Fatal("downstream reached for missing session")
	}
	if store.getCalls != 1 {
		t.Errorf("expected 1 Get call, got %d", store.getCalls)
	}
	if store.touchCalls != 0 {
		t.Errorf("Touch ran on a missing session: %d", store.touchCalls)
	}
}

func TestRequireMasterAuth_SessionExpired_RedirectsToLogin(t *testing.T) {
	store := newFakeSessionStore()
	store.getErr = mastermfa.ErrSessionExpired
	dir := newFakeDirectory()
	mw := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions: store, Directory: dir, Logger: silentLogger(),
	})
	d := &downstreamHandler{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/tenant", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: uuid.New().String()})
	mw(d).ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if d.calls != 0 {
		t.Fatal("downstream reached for expired session")
	}
}

func TestRequireMasterAuth_SessionGetTransientError_Returns500(t *testing.T) {
	store := newFakeSessionStore()
	store.getErr = errors.New("pgx: connection lost")
	dir := newFakeDirectory()
	mw := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions: store, Directory: dir, Logger: silentLogger(),
	})
	d := &downstreamHandler{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/tenant", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: uuid.New().String()})
	mw(d).ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", w.Code)
	}
	if d.calls != 0 {
		t.Fatal("downstream reached on storage failure (deny-by-default)")
	}
}

// ---------------------------------------------------------------------------
// Touch outcomes
// ---------------------------------------------------------------------------

func TestRequireMasterAuth_TouchNotFound_RedirectsToLogin(t *testing.T) {
	// A race: another tab logged out between Get and Touch. The middleware
	// MUST treat this as deny-by-default rather than letting the request
	// through with a stale Master in ctx.
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, err := store.Create(context.Background(), uid, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Force Touch to fail with not-found.
	store.touchErr = mastermfa.ErrSessionNotFound

	dir := newFakeDirectory()
	dir.emails[uid] = "ops@example.com"
	mw := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions: store, Directory: dir, Logger: silentLogger(),
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
		t.Fatal("downstream reached when Touch found no session")
	}
}

func TestRequireMasterAuth_TouchTransientError_Returns500(t *testing.T) {
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	store.touchErr = errors.New("pgx: deadlock")
	dir := newFakeDirectory()
	dir.emails[uid] = "ops@example.com"
	mw := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions: store, Directory: dir, Logger: silentLogger(),
	})
	d := &downstreamHandler{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/tenant", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sess.ID.String()})
	mw(d).ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Directory outcomes
// ---------------------------------------------------------------------------

func TestRequireMasterAuth_DirectoryFailure_Returns500(t *testing.T) {
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	dir := newFakeDirectory()
	dir.err = errors.New("pgx: read-only")
	mw := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions: store, Directory: dir, Logger: silentLogger(),
	})
	d := &downstreamHandler{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/tenant", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sess.ID.String()})
	mw(d).ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Allow path
// ---------------------------------------------------------------------------

func TestRequireMasterAuth_AllowsValidSession(t *testing.T) {
	store := newFakeSessionStore()
	uid := uuid.New()
	sess, _ := store.Create(context.Background(), uid, time.Hour)
	dir := newFakeDirectory()
	dir.emails[uid] = "ops@example.com"
	mw := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions: store, Directory: dir, IdleTTL: 5 * time.Minute,
	})
	d := &downstreamHandler{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/tenant", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sess.ID.String()})
	mw(d).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	if d.calls != 1 {
		t.Fatalf("downstream calls: got %d want 1", d.calls)
	}
	if !d.lastHadCtx {
		t.Fatal("downstream got no Master in ctx")
	}
	if d.lastMaster.ID != uid {
		t.Errorf("master id: got %v want %v", d.lastMaster.ID, uid)
	}
	if d.lastMaster.Email != "ops@example.com" {
		t.Errorf("master email: got %q want %q", d.lastMaster.Email, "ops@example.com")
	}
	if store.touchCalls != 1 {
		t.Errorf("Touch calls: got %d want 1", store.touchCalls)
	}
	if store.lastTouchTTL != 5*time.Minute {
		t.Errorf("Touch TTL: got %v want 5m", store.lastTouchTTL)
	}
}

// ---------------------------------------------------------------------------
// Open-redirect defence on the next= param
// ---------------------------------------------------------------------------

func TestRequireMasterAuth_RejectsAbsoluteNextOnRedirect(t *testing.T) {
	// A hostile request with absolute Path would have RequestURI() that
	// fails isSafeReturnPath; the middleware drops the next param.
	store := newFakeSessionStore()
	dir := newFakeDirectory()
	mw := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions: store, Directory: dir, Logger: silentLogger(),
	})
	d := &downstreamHandler{}
	w := httptest.NewRecorder()
	// Manually craft a request whose URL.RequestURI starts with "//"
	// (scheme-relative). httptest.NewRequest would resolve it; we use a
	// raw http.Request instead.
	r := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Path: "//evil.com/x"},
		Header: http.Header{},
	}
	mw(d).ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	if strings.Contains(loc, "next=") {
		t.Errorf("next= survived an unsafe path: %q", loc)
	}
}
