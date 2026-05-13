package csrf

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
)

// recorder captures whether downstream actually ran and the OnReject
// reason if rejection happened. Tests assert one or the other; both
// firing in a single pass would be a contract bug.
type recorder struct {
	mu          sync.Mutex
	downstream  atomic.Bool
	rejectCount int
	lastReason  Reason
}

func (rec *recorder) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rec.downstream.Store(true)
		w.WriteHeader(http.StatusOK)
	})
}

func (rec *recorder) OnReject(_ *http.Request, reason Reason) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.rejectCount++
	rec.lastReason = reason
}

func newRequest(method string, body string, opts ...func(*http.Request)) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, "https://acme.crm.local/tenant/foo", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, "https://acme.crm.local/tenant/foo", nil)
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

func withCookie(name, value string) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: name, Value: value}) }
}

func withHeader(name, value string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(name, value) }
}

func newCfg(rec *recorder, sessionToken string, sessionErr error, allowedHosts []string, skip func(*http.Request) bool) Config {
	return Config{
		SessionToken: func(_ *http.Request) (string, error) {
			if sessionErr != nil {
				return "", sessionErr
			}
			return sessionToken, nil
		},
		AllowedHosts: func(_ *http.Request) []string { return allowedHosts },
		Skip:         skip,
		OnReject:     rec.OnReject,
	}
}

// TestSafeMethodsBypass — GET/HEAD/OPTIONS skip the check, no cookie
// required. ADR 0073 D1 step 1.
func TestSafeMethodsBypass(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			rec := &recorder{}
			h := New(newCfg(rec, "tok", nil, []string{"acme.crm.local"}, nil))(rec.Handler())
			w := httptest.NewRecorder()
			h.ServeHTTP(w, newRequest(method, ""))
			if !rec.downstream.Load() {
				t.Fatalf("safe method %s should pass through", method)
			}
			if rec.rejectCount != 0 {
				t.Fatalf("OnReject fired on safe method")
			}
		})
	}
}

// TestSkipListBypass — webhook routes (HMAC-authed) explicitly skip.
// ADR 0073 D1 step 2.
func TestSkipListBypass(t *testing.T) {
	rec := &recorder{}
	skip := func(r *http.Request) bool { return r.URL.Path == "/tenant/foo" }
	h := New(newCfg(rec, "tok", nil, []string{"acme.crm.local"}, skip))(rec.Handler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(http.MethodPost, ""))
	if !rec.downstream.Load() {
		t.Fatalf("skip-listed POST should pass through")
	}
}

// TestPOST_NoCookie — acceptance criterion #1: POST /tenant/foo with no
// CSRF cookie returns 403, downstream never runs.
func TestPOST_NoCookie_403(t *testing.T) {
	rec := &recorder{}
	h := New(newCfg(rec, "tok", nil, []string{"acme.crm.local"}, nil))(rec.Handler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(http.MethodPost, ""))
	if rec.downstream.Load() {
		t.Fatalf("downstream should NOT have run")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if rec.lastReason != ReasonCookieMissing {
		t.Fatalf("reason = %q, want %q", rec.lastReason, ReasonCookieMissing)
	}
}

func TestPOST_CookiePresent_TokenMissing_403(t *testing.T) {
	rec := &recorder{}
	h := New(newCfg(rec, "tok", nil, []string{"acme.crm.local"}, nil))(rec.Handler())
	w := httptest.NewRecorder()
	r := newRequest(http.MethodPost, "", withCookie(sessioncookie.NameCSRF, "tok"), withHeader("Origin", "https://acme.crm.local"))
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if rec.lastReason != ReasonTokenMissing {
		t.Fatalf("reason = %q, want %q", rec.lastReason, ReasonTokenMissing)
	}
}

func TestPOST_CookieAndHeaderMatch_OriginAllowed_OK(t *testing.T) {
	rec := &recorder{}
	h := New(newCfg(rec, "tok", nil, []string{"acme.crm.local"}, nil))(rec.Handler())
	w := httptest.NewRecorder()
	r := newRequest(
		http.MethodPost, "",
		withCookie(sessioncookie.NameCSRF, "tok"),
		withHeader(HeaderName, "tok"),
		withHeader("Origin", "https://acme.crm.local"),
	)
	h.ServeHTTP(w, r)
	if !rec.downstream.Load() {
		t.Fatalf("downstream should have run; status=%d, reason=%q", w.Code, rec.lastReason)
	}
}

func TestPOST_CookieAndFormMatch_OriginAllowed_OK(t *testing.T) {
	rec := &recorder{}
	h := New(newCfg(rec, "tok", nil, []string{"acme.crm.local"}, nil))(rec.Handler())
	w := httptest.NewRecorder()
	r := newRequest(
		http.MethodPost, FormField+"=tok",
		withCookie(sessioncookie.NameCSRF, "tok"),
		withHeader("Origin", "https://acme.crm.local"),
	)
	h.ServeHTTP(w, r)
	if !rec.downstream.Load() {
		t.Fatalf("downstream should have run; status=%d, reason=%q", w.Code, rec.lastReason)
	}
}

func TestPOST_CookieMismatchPresented_403(t *testing.T) {
	rec := &recorder{}
	h := New(newCfg(rec, "tok-session", nil, []string{"acme.crm.local"}, nil))(rec.Handler())
	w := httptest.NewRecorder()
	// cookie says "cookie-token" but presented header says "wrong"
	r := newRequest(
		http.MethodPost, "",
		withCookie(sessioncookie.NameCSRF, "cookie-token"),
		withHeader(HeaderName, "wrong"),
		withHeader("Origin", "https://acme.crm.local"),
	)
	h.ServeHTTP(w, r)
	if rec.lastReason != ReasonTokenMismatch {
		t.Fatalf("reason = %q, want %q", rec.lastReason, ReasonTokenMismatch)
	}
}

func TestPOST_PresentedMismatchSession_403(t *testing.T) {
	// cookie == presented but session.csrf_token differs (the session
	// was rotated between cookie write and this request).
	rec := &recorder{}
	h := New(newCfg(rec, "session-token", nil, []string{"acme.crm.local"}, nil))(rec.Handler())
	w := httptest.NewRecorder()
	r := newRequest(
		http.MethodPost, "",
		withCookie(sessioncookie.NameCSRF, "stale"),
		withHeader(HeaderName, "stale"),
		withHeader("Origin", "https://acme.crm.local"),
	)
	h.ServeHTTP(w, r)
	if rec.lastReason != ReasonSessionTokenMismatch {
		t.Fatalf("reason = %q, want %q", rec.lastReason, ReasonSessionTokenMismatch)
	}
}

func TestPOST_SessionLookupError_403(t *testing.T) {
	rec := &recorder{}
	h := New(newCfg(rec, "", errors.New("no session"), []string{"acme.crm.local"}, nil))(rec.Handler())
	w := httptest.NewRecorder()
	r := newRequest(
		http.MethodPost, "",
		withCookie(sessioncookie.NameCSRF, "tok"),
		withHeader(HeaderName, "tok"),
		withHeader("Origin", "https://acme.crm.local"),
	)
	h.ServeHTTP(w, r)
	if rec.lastReason != ReasonSessionLookup {
		t.Fatalf("reason = %q, want %q", rec.lastReason, ReasonSessionLookup)
	}
}

func TestPOST_SessionTokenEmpty_403(t *testing.T) {
	rec := &recorder{}
	// session lookup returns empty string + nil err — programmer bug we
	// must surface as 403 (NOT silent pass).
	h := New(newCfg(rec, "", nil, []string{"acme.crm.local"}, nil))(rec.Handler())
	w := httptest.NewRecorder()
	r := newRequest(
		http.MethodPost, "",
		withCookie(sessioncookie.NameCSRF, "tok"),
		withHeader(HeaderName, "tok"),
		withHeader("Origin", "https://acme.crm.local"),
	)
	h.ServeHTTP(w, r)
	if rec.lastReason != ReasonSessionTokenMissing {
		t.Fatalf("reason = %q, want %q", rec.lastReason, ReasonSessionTokenMissing)
	}
}

// TestPOST_AttackerOrigin — acceptance criterion #2: a request whose
// Origin is a foreign attacker host returns 403 even if the CSRF token
// matches.
func TestPOST_AttackerOrigin_403(t *testing.T) {
	rec := &recorder{}
	h := New(newCfg(rec, "tok", nil, []string{"acme.crm.local"}, nil))(rec.Handler())
	w := httptest.NewRecorder()
	r := newRequest(
		http.MethodPost, "",
		withCookie(sessioncookie.NameCSRF, "tok"),
		withHeader(HeaderName, "tok"),
		withHeader("Origin", "https://attacker.example"),
	)
	h.ServeHTTP(w, r)
	if rec.downstream.Load() {
		t.Fatalf("attacker origin must NOT pass through")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if rec.lastReason != ReasonOriginMismatch {
		t.Fatalf("reason = %q, want %q", rec.lastReason, ReasonOriginMismatch)
	}
}

func TestPOST_RefererFallback_OK(t *testing.T) {
	rec := &recorder{}
	h := New(newCfg(rec, "tok", nil, []string{"acme.crm.local"}, nil))(rec.Handler())
	w := httptest.NewRecorder()
	r := newRequest(
		http.MethodPost, "",
		withCookie(sessioncookie.NameCSRF, "tok"),
		withHeader(HeaderName, "tok"),
		withHeader("Referer", "https://acme.crm.local/some/page"),
	)
	h.ServeHTTP(w, r)
	if !rec.downstream.Load() {
		t.Fatalf("Referer fallback should pass when Origin is absent; got reason=%q", rec.lastReason)
	}
}

func TestPOST_OriginAndRefererMissing_403(t *testing.T) {
	rec := &recorder{}
	h := New(newCfg(rec, "tok", nil, []string{"acme.crm.local"}, nil))(rec.Handler())
	w := httptest.NewRecorder()
	r := newRequest(
		http.MethodPost, "",
		withCookie(sessioncookie.NameCSRF, "tok"),
		withHeader(HeaderName, "tok"),
	)
	h.ServeHTTP(w, r)
	if rec.lastReason != ReasonOriginMissing {
		t.Fatalf("reason = %q, want %q", rec.lastReason, ReasonOriginMissing)
	}
}

func TestPOST_OriginNullTreatedAsMissing(t *testing.T) {
	// "null" Origin is what browsers send for sandboxed iframes /
	// data: contexts. Treat as missing so the Referer fallback runs;
	// if Referer is also absent, reject.
	rec := &recorder{}
	h := New(newCfg(rec, "tok", nil, []string{"acme.crm.local"}, nil))(rec.Handler())
	w := httptest.NewRecorder()
	r := newRequest(
		http.MethodPost, "",
		withCookie(sessioncookie.NameCSRF, "tok"),
		withHeader(HeaderName, "tok"),
		withHeader("Origin", "null"),
	)
	h.ServeHTTP(w, r)
	if rec.lastReason != ReasonOriginMissing {
		t.Fatalf("reason = %q, want %q", rec.lastReason, ReasonOriginMissing)
	}
}

func TestPOST_OriginMalformed_403(t *testing.T) {
	rec := &recorder{}
	h := New(newCfg(rec, "tok", nil, []string{"acme.crm.local"}, nil))(rec.Handler())
	w := httptest.NewRecorder()
	r := newRequest(
		http.MethodPost, "",
		withCookie(sessioncookie.NameCSRF, "tok"),
		withHeader(HeaderName, "tok"),
		withHeader("Origin", "://not a url"),
	)
	h.ServeHTTP(w, r)
	if rec.lastReason != ReasonOriginMismatch {
		t.Fatalf("reason = %q, want %q", rec.lastReason, ReasonOriginMismatch)
	}
}

func TestPOST_RefererMalformed_403(t *testing.T) {
	rec := &recorder{}
	h := New(newCfg(rec, "tok", nil, []string{"acme.crm.local"}, nil))(rec.Handler())
	w := httptest.NewRecorder()
	r := newRequest(
		http.MethodPost, "",
		withCookie(sessioncookie.NameCSRF, "tok"),
		withHeader(HeaderName, "tok"),
		withHeader("Referer", "://not a url"),
	)
	h.ServeHTTP(w, r)
	if rec.lastReason != ReasonOriginMismatch {
		t.Fatalf("reason = %q, want %q", rec.lastReason, ReasonOriginMismatch)
	}
}

func TestNew_PanicsOnNilSessionToken(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic for nil SessionToken")
		}
	}()
	_ = New(Config{
		AllowedHosts: func(*http.Request) []string { return nil },
	})
}

func TestNew_PanicsOnNilAllowedHosts(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic for nil AllowedHosts")
		}
	}()
	_ = New(Config{
		SessionToken: func(*http.Request) (string, error) { return "tok", nil },
	})
}

func TestPOST_OriginCaseInsensitive(t *testing.T) {
	// The host comparison is case-insensitive (RFC 4343 — DNS
	// case-insensitive). Allowlist with lowercase, request with mixed
	// case → still passes.
	rec := &recorder{}
	h := New(newCfg(rec, "tok", nil, []string{"acme.crm.local"}, nil))(rec.Handler())
	w := httptest.NewRecorder()
	r := newRequest(
		http.MethodPost, "",
		withCookie(sessioncookie.NameCSRF, "tok"),
		withHeader(HeaderName, "tok"),
		withHeader("Origin", "https://Acme.CRM.local"),
	)
	h.ServeHTTP(w, r)
	if !rec.downstream.Load() {
		t.Fatalf("case-insensitive origin compare should pass; got reason=%q", rec.lastReason)
	}
}

// TestIntegration_AcceptanceCriteria1and2_NoDBWrite is the closest we
// can get without a wired-up DB — when the middleware rejects, the
// downstream handler (which would have written) is never invoked.
// Acceptance criterion #1 / #2 explicitly require "nothing written to
// the DB" on rejection; this asserts the gate is in front of any
// handler side-effect.
func TestIntegration_AcceptanceCriteria1and2_NoDBWrite(t *testing.T) {
	rec := &recorder{}
	dbWrites := atomic.Int32{}
	mw := New(newCfg(rec, "tok", nil, []string{"acme.crm.local"}, nil))
	final := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dbWrites.Add(1) // stand-in for any write a handler would do
		w.WriteHeader(http.StatusOK)
	})

	// Case A: no token → rejected, no write
	wA := httptest.NewRecorder()
	mw(final).ServeHTTP(wA, newRequest(http.MethodPost, ""))
	if wA.Code != http.StatusForbidden || dbWrites.Load() != 0 {
		t.Fatalf("no-token POST should not reach handler: status=%d writes=%d", wA.Code, dbWrites.Load())
	}

	// Case B: attacker Origin → rejected, no write
	wB := httptest.NewRecorder()
	mw(final).ServeHTTP(wB, newRequest(
		http.MethodPost, "",
		withCookie(sessioncookie.NameCSRF, "tok"),
		withHeader(HeaderName, "tok"),
		withHeader("Origin", "https://attacker.example"),
	))
	if wB.Code != http.StatusForbidden || dbWrites.Load() != 0 {
		t.Fatalf("attacker-Origin POST should not reach handler: status=%d writes=%d", wB.Code, dbWrites.Load())
	}

	// Case C: legitimate request with all three layers → passes,
	// downstream runs once.
	wC := httptest.NewRecorder()
	mw(final).ServeHTTP(wC, newRequest(
		http.MethodPost, "",
		withCookie(sessioncookie.NameCSRF, "tok"),
		withHeader(HeaderName, "tok"),
		withHeader("Origin", "https://acme.crm.local"),
	))
	if wC.Code != http.StatusOK || dbWrites.Load() != 1 {
		t.Fatalf("good request should reach handler exactly once: status=%d writes=%d", wC.Code, dbWrites.Load())
	}
}
