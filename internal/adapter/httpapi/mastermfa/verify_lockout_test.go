package mastermfa_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// SIN-62380 (CAVEAT-3 of SIN-62343): the verify handler's session-
// scoped 5-strike lockout invalidates the master session, fires a
// Slack alert, and redirects to /m/login. These tests exercise the
// lockout flow with in-memory fakes.

// fakeFailureCounter is an in-memory mastermfa.VerifyFailureCounter.
// It is intentionally narrower than the real Redis adapter — no TTL,
// no namespacing — because the tests assert the handler's contract
// with the port, not the adapter's persistence.
type fakeFailureCounter struct {
	mu sync.Mutex

	counts        map[uuid.UUID]int
	incrementErr  error
	resetErr      error
	incrementHits int
	resetHits     int
	lastIncrement uuid.UUID
	lastReset     uuid.UUID
}

func newFakeFailureCounter() *fakeFailureCounter {
	return &fakeFailureCounter{counts: make(map[uuid.UUID]int)}
}

func (f *fakeFailureCounter) Increment(_ context.Context, sessionID uuid.UUID) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.incrementHits++
	f.lastIncrement = sessionID
	if f.incrementErr != nil {
		return 0, f.incrementErr
	}
	f.counts[sessionID]++
	return f.counts[sessionID], nil
}

func (f *fakeFailureCounter) Reset(_ context.Context, sessionID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetHits++
	f.lastReset = sessionID
	if f.resetErr != nil {
		return f.resetErr
	}
	delete(f.counts, sessionID)
	return nil
}

// fakeInvalidator records Invalidate calls. It always clears the
// cookie via sessioncookie.ClearMaster so the response carries the
// scrubbed Set-Cookie header — matching the production HTTPSession
// behaviour.
type fakeInvalidator struct {
	calls int
	err   error
}

func (f *fakeInvalidator) Invalidate(w http.ResponseWriter, _ *http.Request) error {
	f.calls++
	sessioncookie.ClearMaster(w)
	if f.err != nil {
		return f.err
	}
	return nil
}

// fakeLockoutAlerter records AlertVerifyLockout calls and the details
// they were invoked with. Tests assert the handler threads the right
// fields (UserID / SessionID / failure count / IP / UA / route).
type fakeLockoutAlerter struct {
	calls   int
	err     error
	details mastermfa.VerifyLockoutDetails
}

func (f *fakeLockoutAlerter) AlertVerifyLockout(_ context.Context, details mastermfa.VerifyLockoutDetails) error {
	f.calls++
	f.details = details
	if f.err != nil {
		return f.err
	}
	return nil
}

// newLockoutVerifyHandler builds a verify handler wired to the SIN-62380
// collaborators. Threshold defaults to mastermfa.LockoutThresholdDefault
// (5) when zero is passed.
func newLockoutVerifyHandler(t *testing.T, v *fakeVerifier, c *fakeConsumer, sessions *fakeSessions, counter *fakeFailureCounter, inv *fakeInvalidator, alerter *fakeLockoutAlerter, threshold int) *mastermfa.VerifyHandler {
	t.Helper()
	return mastermfa.NewVerifyHandler(mastermfa.VerifyHandlerConfig{
		Verifier:         v,
		Consumer:         c,
		Sessions:         sessions,
		Failures:         counter,
		Invalidator:      inv,
		Alerter:          alerter,
		LockoutThreshold: threshold,
		FallbackOK:       "/m/dashboard",
		LoginPath:        "/m/login",
	})
}

// withCookiedRequest stamps the master session cookie onto a verify-
// post request so the lockout machinery has a session id to key the
// counter on. Returns the (request, sessionID) pair.
func withCookiedRequest(t *testing.T, body url.Values) (*http.Request, uuid.UUID) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	uid := uuid.New()
	sid := uuid.New()
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sid.String()})
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uid, Email: "ops@example.com"}))
	return r, sid
}

func TestVerifyHandler_Lockout_TOTPInvalidIncrementsCounter(t *testing.T) {
	v := &fakeVerifier{err: mfa.ErrInvalidCode}
	counter := newFakeFailureCounter()
	inv := &fakeInvalidator{}
	alerter := &fakeLockoutAlerter{}
	h := newLockoutVerifyHandler(t, v, &fakeConsumer{}, &fakeSessions{}, counter, inv, alerter, 0)

	r, sid := withCookiedRequest(t, url.Values{"code": {"000000"}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401 (re-render with error)", w.Code)
	}
	if counter.incrementHits != 1 {
		t.Errorf("Increment calls: got %d want 1", counter.incrementHits)
	}
	if counter.lastIncrement != sid {
		t.Errorf("Increment session id: got %s want %s", counter.lastIncrement, sid)
	}
	if inv.calls != 0 {
		t.Errorf("Invalidate calls below threshold: got %d want 0", inv.calls)
	}
	if alerter.calls != 0 {
		t.Errorf("Alert calls below threshold: got %d want 0", alerter.calls)
	}
	if !strings.Contains(w.Body.String(), "código inválido") {
		t.Errorf("body missing generic error message")
	}
}

func TestVerifyHandler_Lockout_RecoveryInvalidIncrementsCounter(t *testing.T) {
	c := &fakeConsumer{err: mfa.ErrInvalidCode}
	counter := newFakeFailureCounter()
	inv := &fakeInvalidator{}
	alerter := &fakeLockoutAlerter{}
	h := newLockoutVerifyHandler(t, &fakeVerifier{}, c, &fakeSessions{}, counter, inv, alerter, 0)

	r, sid := withCookiedRequest(t, url.Values{"code": {"ZZZZZ-77777"}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if counter.incrementHits != 1 {
		t.Errorf("Increment calls: got %d want 1", counter.incrementHits)
	}
	if counter.lastIncrement != sid {
		t.Errorf("Increment session id mismatch")
	}
	if inv.calls != 0 || alerter.calls != 0 {
		t.Errorf("trip path fired below threshold")
	}
}

func TestVerifyHandler_Lockout_EmptyCodeIncrementsCounter(t *testing.T) {
	// An empty submission is also a strike — otherwise an attacker
	// could indefinitely reset the implicit "verify in progress" UI
	// without ever using a strike.
	counter := newFakeFailureCounter()
	inv := &fakeInvalidator{}
	alerter := &fakeLockoutAlerter{}
	h := newLockoutVerifyHandler(t, &fakeVerifier{}, &fakeConsumer{}, &fakeSessions{}, counter, inv, alerter, 0)

	r, _ := withCookiedRequest(t, url.Values{"code": {""}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if counter.incrementHits != 1 {
		t.Errorf("Increment on empty code: got %d want 1", counter.incrementHits)
	}
}

func TestVerifyHandler_Lockout_TripsAtThresholdInvalidatesAndAlerts(t *testing.T) {
	v := &fakeVerifier{err: mfa.ErrInvalidCode}
	counter := newFakeFailureCounter()
	inv := &fakeInvalidator{}
	alerter := &fakeLockoutAlerter{}
	h := newLockoutVerifyHandler(t, v, &fakeConsumer{}, &fakeSessions{}, counter, inv, alerter, 0)

	// Pre-load the counter to threshold-1 so the next strike trips.
	r, sid := withCookiedRequest(t, url.Values{"code": {"000000"}})
	for i := 0; i < mastermfa.LockoutThresholdDefault-1; i++ {
		w := httptest.NewRecorder()
		// Re-build the request each iteration so headers/body don't carry over.
		req, _ := withCookiedRequestWithSession(t, sid, url.Values{"code": {"000000"}})
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("iteration %d: status %d want 401", i, w.Code)
		}
	}

	// Trip strike.
	w := httptest.NewRecorder()
	r.RemoteAddr = "203.0.113.10:55555"
	r.Header.Set("User-Agent", "curl/8.0.1")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("trip status: got %d want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/m/login" {
		t.Errorf("Location: got %q want /m/login", loc)
	}
	if inv.calls != 1 {
		t.Errorf("Invalidate calls: got %d want 1", inv.calls)
	}
	if alerter.calls != 1 {
		t.Fatalf("Alert calls: got %d want 1", alerter.calls)
	}
	if counter.incrementHits != mastermfa.LockoutThresholdDefault {
		t.Errorf("Increment calls: got %d want %d", counter.incrementHits, mastermfa.LockoutThresholdDefault)
	}
	if alerter.details.SessionID != sid {
		t.Errorf("Alert session id: got %s want %s", alerter.details.SessionID, sid)
	}
	if alerter.details.Failures != mastermfa.LockoutThresholdDefault {
		t.Errorf("Alert failures: got %d want %d", alerter.details.Failures, mastermfa.LockoutThresholdDefault)
	}
	if alerter.details.IP != "203.0.113.10" {
		t.Errorf("Alert IP: got %q want 203.0.113.10 (port stripped)", alerter.details.IP)
	}
	if alerter.details.UserAgent != "curl/8.0.1" {
		t.Errorf("Alert UA: got %q", alerter.details.UserAgent)
	}
	if alerter.details.Route != "/m/2fa/verify" {
		t.Errorf("Alert Route: got %q", alerter.details.Route)
	}
	// The cookie should have been cleared by the invalidator.
	cookies := w.Result().Cookies()
	cleared := false
	for _, c := range cookies {
		if c.Name == sessioncookie.NameMaster && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("master cookie was not cleared on lockout trip; cookies=%v", cookies)
	}
	// Counter is reset on the trip so a re-login on the same browser
	// does not inherit the stale strike count.
	if counter.resetHits != 1 {
		t.Errorf("Reset calls on trip: got %d want 1", counter.resetHits)
	}
}

// withCookiedRequestWithSession mints a verify-post request carrying
// the provided session id. Used in the loop above so the counter is
// keyed on the same id across iterations.
func withCookiedRequestWithSession(t *testing.T, sid uuid.UUID, body url.Values) (*http.Request, uuid.UUID) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	uid := uuid.New()
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sid.String()})
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uid, Email: "ops@example.com"}))
	return r, sid
}

func TestVerifyHandler_Lockout_HappyPathResetsCounter(t *testing.T) {
	v := &fakeVerifier{}
	counter := newFakeFailureCounter()
	inv := &fakeInvalidator{}
	alerter := &fakeLockoutAlerter{}
	h := newLockoutVerifyHandler(t, v, &fakeConsumer{}, &fakeSessions{}, counter, inv, alerter, 0)

	r, sid := withCookiedRequest(t, url.Values{"code": {"287082"}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if counter.incrementHits != 0 {
		t.Errorf("Increment calls on happy path: got %d want 0", counter.incrementHits)
	}
	if counter.resetHits != 1 {
		t.Errorf("Reset calls on happy path: got %d want 1", counter.resetHits)
	}
	if counter.lastReset != sid {
		t.Errorf("Reset session id: got %s want %s", counter.lastReset, sid)
	}
}

func TestVerifyHandler_Lockout_CounterErrorReturns500(t *testing.T) {
	// A Redis blip on Increment MUST stop the verify path — silently
	// disabling the lockout would defeat the fix.
	counter := newFakeFailureCounter()
	counter.incrementErr = errors.New("redis: i/o timeout")
	v := &fakeVerifier{err: mfa.ErrInvalidCode}
	inv := &fakeInvalidator{}
	alerter := &fakeLockoutAlerter{}
	h := newLockoutVerifyHandler(t, v, &fakeConsumer{}, &fakeSessions{}, counter, inv, alerter, 0)

	r, _ := withCookiedRequest(t, url.Values{"code": {"000000"}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500 (counter outage)", w.Code)
	}
	if inv.calls != 0 {
		t.Errorf("Invalidate ran on counter outage: %d", inv.calls)
	}
	if alerter.calls != 0 {
		t.Errorf("Alert ran on counter outage: %d", alerter.calls)
	}
}

func TestVerifyHandler_Lockout_NoCookieFallsThroughLegacy(t *testing.T) {
	// No cookie on the request means the upstream master-auth
	// middleware would have already redirected; reaching the verify
	// handler in that state is a wiring edge. Falling through to the
	// legacy 401 re-render is the right answer — the lockout has no
	// session id to key the counter on, so disabling it for that
	// request is benign (the next request still goes through the
	// auth gate).
	counter := newFakeFailureCounter()
	inv := &fakeInvalidator{}
	alerter := &fakeLockoutAlerter{}
	v := &fakeVerifier{err: mfa.ErrInvalidCode}
	h := newLockoutVerifyHandler(t, v, &fakeConsumer{}, &fakeSessions{}, counter, inv, alerter, 0)

	body := url.Values{"code": {"000000"}}
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	uid := uuid.New()
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uid, Email: "ops"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", w.Code)
	}
	if counter.incrementHits != 0 || inv.calls != 0 || alerter.calls != 0 {
		t.Errorf("lockout machinery ran without a cookie: incr=%d inv=%d alert=%d",
			counter.incrementHits, inv.calls, alerter.calls)
	}
}

func TestVerifyHandler_Lockout_ThresholdConfigurable(t *testing.T) {
	v := &fakeVerifier{err: mfa.ErrInvalidCode}
	counter := newFakeFailureCounter()
	inv := &fakeInvalidator{}
	alerter := &fakeLockoutAlerter{}
	// Threshold 2 — second strike trips.
	h := newLockoutVerifyHandler(t, v, &fakeConsumer{}, &fakeSessions{}, counter, inv, alerter, 2)

	sid := uuid.New()
	for i := 0; i < 1; i++ {
		req, _ := withCookiedRequestWithSession(t, sid, url.Values{"code": {"000000"}})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("strike %d status: got %d want 401", i, w.Code)
		}
	}
	// Second strike trips.
	req, _ := withCookiedRequestWithSession(t, sid, url.Values{"code": {"000000"}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("trip status: got %d want 303", w.Code)
	}
	if alerter.details.Failures != 2 {
		t.Errorf("Alert failures: got %d want 2", alerter.details.Failures)
	}
}

func TestVerifyHandler_Lockout_NotWiredFallsBackToLegacy(t *testing.T) {
	// Half-wired (only Failures, no Invalidator / Alerter) MUST fall
	// through to the legacy re-render path — a typo in cmd/server
	// should not silently disable the lockout while pretending to
	// enforce it.
	counter := newFakeFailureCounter()
	v := &fakeVerifier{err: mfa.ErrInvalidCode}
	h := mastermfa.NewVerifyHandler(mastermfa.VerifyHandlerConfig{
		Verifier:   v,
		Consumer:   &fakeConsumer{},
		Sessions:   &fakeSessions{},
		Failures:   counter, // Invalidator + Alerter omitted
		FallbackOK: "/m/dashboard",
	})

	r, _ := withCookiedRequest(t, url.Values{"code": {"000000"}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", w.Code)
	}
	if counter.incrementHits != 0 {
		t.Errorf("Increment ran on half-wired lockout: %d", counter.incrementHits)
	}
}

func TestVerifyHandler_Lockout_AlertFailureDoesNotAbortRedirect(t *testing.T) {
	// The persisted invalidation is the authoritative penalty; the
	// alert is the notification side-effect (consistent with
	// iam.Service.Login + ratelimit.Alerter). A Slack outage MUST
	// NOT leave the user inside a half-broken state.
	v := &fakeVerifier{err: mfa.ErrInvalidCode}
	counter := newFakeFailureCounter()
	inv := &fakeInvalidator{}
	alerter := &fakeLockoutAlerter{err: errors.New("slack: 503")}
	h := newLockoutVerifyHandler(t, v, &fakeConsumer{}, &fakeSessions{}, counter, inv, alerter, 1)

	r, _ := withCookiedRequest(t, url.Values{"code": {"000000"}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303 (alert failure must not abort redirect)", w.Code)
	}
	if inv.calls != 1 {
		t.Errorf("Invalidate calls: got %d want 1", inv.calls)
	}
}

func TestVerifyHandler_Lockout_InvalidatorFailureStillAlertsAndRedirects(t *testing.T) {
	// A storage failure on Invalidate is logged loudly but does NOT
	// abort the redirect — leaving the user inside a half-broken
	// state would be worse than denying them. The Slack alert still
	// fires so an operator can chase the failed delete.
	v := &fakeVerifier{err: mfa.ErrInvalidCode}
	counter := newFakeFailureCounter()
	inv := &fakeInvalidator{err: errors.New("pgx: deadlock")}
	alerter := &fakeLockoutAlerter{}
	h := newLockoutVerifyHandler(t, v, &fakeConsumer{}, &fakeSessions{}, counter, inv, alerter, 1)

	r, _ := withCookiedRequest(t, url.Values{"code": {"000000"}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if alerter.calls != 1 {
		t.Errorf("Alert calls: got %d want 1", alerter.calls)
	}
}

func TestVerifyHandler_Lockout_UnparseableCookieFallsThroughLegacy(t *testing.T) {
	// An unparseable cookie reaching the verify handler is a wiring
	// edge (master-auth would have redirected). Falling through to
	// the legacy re-render is the safe answer.
	counter := newFakeFailureCounter()
	inv := &fakeInvalidator{}
	alerter := &fakeLockoutAlerter{}
	v := &fakeVerifier{err: mfa.ErrInvalidCode}
	h := newLockoutVerifyHandler(t, v, &fakeConsumer{}, &fakeSessions{}, counter, inv, alerter, 0)

	body := url.Values{"code": {"000000"}}
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: "not-a-uuid"})
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uuid.New(), Email: "ops"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", w.Code)
	}
	if counter.incrementHits != 0 {
		t.Errorf("Increment ran on unparseable cookie")
	}
}

func TestSessionIDExtractor(t *testing.T) {
	t.Run("nil request", func(t *testing.T) {
		if got := mastermfa.SessionIDExtractor(nil); got != "" {
			t.Errorf("nil request: got %q want empty", got)
		}
	})
	t.Run("missing cookie", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", nil)
		if got := mastermfa.SessionIDExtractor(r); got != "" {
			t.Errorf("no cookie: got %q want empty", got)
		}
	})
	t.Run("present cookie", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", nil)
		r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: "the-session-id"})
		if got := mastermfa.SessionIDExtractor(r); got != "the-session-id" {
			t.Errorf("present: got %q want the-session-id", got)
		}
	})
}

func TestMasterUserIDExtractor(t *testing.T) {
	t.Run("nil request", func(t *testing.T) {
		if got := mastermfa.MasterUserIDExtractor(nil); got != "" {
			t.Errorf("nil request: got %q want empty", got)
		}
	})
	t.Run("missing context", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", nil)
		if got := mastermfa.MasterUserIDExtractor(r); got != "" {
			t.Errorf("no master: got %q want empty", got)
		}
	})
	t.Run("present master", func(t *testing.T) {
		uid := uuid.New()
		r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", nil)
		r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uid, Email: "ops"}))
		if got := mastermfa.MasterUserIDExtractor(r); got != uid.String() {
			t.Errorf("present: got %q want %s", got, uid)
		}
	})
}
