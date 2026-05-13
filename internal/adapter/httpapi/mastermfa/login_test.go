package mastermfa_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
)

// fakeMasterLogin tracks calls + returns scripted (session, error).
type fakeMasterLogin struct {
	calls  int
	last   loginCallArgs
	result iam.Session
	err    error
}

type loginCallArgs struct {
	host     string
	email    string
	password string
	ip       net.IP
	ua       string
	route    string
}

func (f *fakeMasterLogin) Login(_ context.Context, host, email, password string, ip net.IP, ua, route string) (iam.Session, error) {
	f.calls++
	f.last = loginCallArgs{host: host, email: email, password: password, ip: ip, ua: ua, route: route}
	return f.result, f.err
}

func newLoginHandlerWith(t *testing.T, login mastermfa.MasterLoginFunc, store mastermfa.SessionStore) *mastermfa.LoginHandler {
	t.Helper()
	return mastermfa.NewLoginHandler(mastermfa.LoginHandlerConfig{
		Login:    login,
		Sessions: store,
		HardTTL:  time.Hour,
		Logger:   silentLogger(),
	})
}

// ---------------------------------------------------------------------------
// Constructor preconditions
// ---------------------------------------------------------------------------

func TestNewLoginHandler_PanicsOnMissingDeps(t *testing.T) {
	cases := map[string]mastermfa.LoginHandlerConfig{
		"nil login": {Sessions: newFakeSessionStore()},
		"nil sessions": {Login: func(context.Context, string, string, string, net.IP, string, string) (iam.Session, error) {
			return iam.Session{}, nil
		}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic, got none")
				}
			}()
			mastermfa.NewLoginHandler(cfg)
		})
	}
}

func TestNewLoginHandler_AppliesDefaults(t *testing.T) {
	// Constructed with only the required fields → defaults fill the rest.
	h := mastermfa.NewLoginHandler(mastermfa.LoginHandlerConfig{
		Login: func(context.Context, string, string, string, net.IP, string, string) (iam.Session, error) {
			return iam.Session{}, nil
		},
		Sessions: newFakeSessionStore(),
	})
	if h == nil {
		t.Fatal("expected handler, got nil")
	}
}

// ---------------------------------------------------------------------------
// GET /m/login
// ---------------------------------------------------------------------------

func TestLoginHandler_GET_RendersForm(t *testing.T) {
	store := newFakeSessionStore()
	login := &fakeMasterLogin{}
	h := newLoginHandlerWith(t, login.Login, store)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/login", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<form method="POST" action="/m/login`) {
		t.Errorf("body missing form: %s", body)
	}
	if strings.Contains(body, `data-testid="login-error"`) {
		t.Errorf("body unexpectedly carried login-error on a fresh GET")
	}
	if w.Header().Get("Cache-Control") == "" {
		t.Errorf("missing Cache-Control on form render")
	}
}

func TestLoginHandler_GET_PreservesSafeNextParam(t *testing.T) {
	// html/template URL-escapes interpolated values inside an attribute
	// (so "/m/grants/foo" becomes "%2fm%2fgrants%2ffoo"). That is the
	// safe default — the browser decodes it back on submit. Assert on
	// the URL-encoded form so the test pins the right escape pipeline.
	store := newFakeSessionStore()
	h := newLoginHandlerWith(t, (&fakeMasterLogin{}).Login, store)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/login?next=/m/grants/foo", nil)
	h.ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, `action="/m/login?next=`) {
		t.Errorf("form action missing next param: %s", body)
	}
	if !strings.Contains(body, `m%2fgrants%2ffoo`) && !strings.Contains(body, `m/grants/foo`) {
		t.Errorf("safe next= not preserved (in either raw or URL-encoded form): %s", body)
	}
}

func TestLoginHandler_GET_DropsUnsafeNextParam(t *testing.T) {
	store := newFakeSessionStore()
	h := newLoginHandlerWith(t, (&fakeMasterLogin{}).Login, store)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/login?next=https://evil.com/x", nil)
	h.ServeHTTP(w, r)
	body := w.Body.String()
	if strings.Contains(body, `evil.com`) {
		t.Errorf("unsafe next= leaked to form: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Method handling
// ---------------------------------------------------------------------------

func TestLoginHandler_RejectsOtherMethods(t *testing.T) {
	store := newFakeSessionStore()
	h := newLoginHandlerWith(t, (&fakeMasterLogin{}).Login, store)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/m/login", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d want 405", w.Code)
	}
	if !strings.Contains(w.Header().Get("Allow"), http.MethodPost) {
		t.Errorf("missing Allow header: %q", w.Header().Get("Allow"))
	}
}

// ---------------------------------------------------------------------------
// POST happy path
// ---------------------------------------------------------------------------

func TestLoginHandler_POST_HappyPath(t *testing.T) {
	store := newFakeSessionStore()
	login := &fakeMasterLogin{
		result: iam.Session{UserID: uuid.New()},
	}
	h := mastermfa.NewLoginHandler(mastermfa.LoginHandlerConfig{
		Login:    login.Login,
		Sessions: store,
		HardTTL:  4 * time.Hour,
		Logger:   silentLogger(),
	})
	form := url.Values{}
	form.Set("email", "ops@example.com")
	form.Set("password", "correct horse")
	body := strings.NewReader(form.Encode())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/m/login", body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "203.0.113.5:12345"
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/m/2fa/verify" {
		t.Errorf("Location: got %q want /m/2fa/verify", loc)
	}

	// Cookie set with Secure / HttpOnly / SameSite=Strict / __Host- prefix.
	cookies := w.Result().Cookies()
	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == sessioncookie.NameMaster {
			found = c
		}
	}
	if found == nil {
		t.Fatalf("__Host-sess-master cookie not set; got %v", cookies)
	}
	if !found.Secure || !found.HttpOnly || found.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie flags: got %+v", found)
	}
	if found.MaxAge != int((4 * time.Hour).Seconds()) {
		t.Errorf("cookie max-age: got %d want %d", found.MaxAge, int((4 * time.Hour).Seconds()))
	}
	if _, err := uuid.Parse(found.Value); err != nil {
		t.Errorf("cookie value not a uuid: %q", found.Value)
	}

	// Master session row created with the right user + ttl.
	if store.createCalls != 1 {
		t.Errorf("Create calls: got %d want 1", store.createCalls)
	}
	if store.lastCreateUserID != login.result.UserID {
		t.Errorf("Create userID: got %v want %v", store.lastCreateUserID, login.result.UserID)
	}
	// SIN-62379: r.URL.Path threads through to the iam.Service.Login
	// call so the master-lockout alert can carry route per ADR 0074 §6.
	if login.last.route != "/m/login" {
		t.Errorf("Login route: got %q want /m/login", login.last.route)
	}
	if store.lastCreateTTL != 4*time.Hour {
		t.Errorf("Create ttl: got %v want 4h", store.lastCreateTTL)
	}

	// Login was called with the form values + remote IP.
	if login.last.email != "ops@example.com" {
		t.Errorf("login email: got %q", login.last.email)
	}
	if login.last.password != "correct horse" {
		t.Errorf("login password not forwarded")
	}
	if login.last.ip == nil || login.last.ip.String() != "203.0.113.5" {
		t.Errorf("login ip: got %v", login.last.ip)
	}
}

func TestLoginHandler_POST_PropagatesNextToVerify(t *testing.T) {
	store := newFakeSessionStore()
	login := &fakeMasterLogin{result: iam.Session{UserID: uuid.New()}}
	h := newLoginHandlerWith(t, login.Login, store)

	form := url.Values{}
	form.Set("email", "ops@example.com")
	form.Set("password", "x")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/m/login?next=/m/tenant/foo", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location %q: %v", loc, err)
	}
	if parsed.Path != "/m/2fa/verify" {
		t.Errorf("Location path: got %q want /m/2fa/verify", parsed.Path)
	}
	if got := parsed.Query().Get("return"); got != "/m/tenant/foo" {
		t.Errorf("return param: got %q want /m/tenant/foo (raw Location: %q)", got, loc)
	}
}

// TestLoginHandler_POST_RedirectReturnRoundTripsThroughVerifyParse pins the
// SIN-62394 fix: when ?next= carries a path with embedded query chars (`&`,
// `?`, `=`), the redirect's `?return=` value must URL-encode them so the
// verify handler's r.URL.Query().Get("return") decodes back to the exact
// original path+query — bare concatenation would cause the parser to split
// on the first `&` and silently drop the rest of the query.
func TestLoginHandler_POST_RedirectReturnRoundTripsThroughVerifyParse(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "single query pair", raw: "/m/users?filter=active"},
		{name: "ampersand-joined pairs", raw: "/m/users?filter=active&page=2"},
		{name: "issue example", raw: "/foo?bar=baz&qux=1"},
		{name: "multiple ampersands and equals", raw: "/m/grants?status=open&owner=alice&page=3&sort=asc"},
		{name: "no query string", raw: "/m/dashboard"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeSessionStore()
			login := &fakeMasterLogin{result: iam.Session{UserID: uuid.New()}}
			h := newLoginHandlerWith(t, login.Login, store)

			form := url.Values{}
			form.Set("email", "ops@example.com")
			form.Set("password", "x")
			loginURL := "/m/login?" + url.Values{"next": []string{tc.raw}}.Encode()

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, loginURL, strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			h.ServeHTTP(w, r)

			if w.Code != http.StatusSeeOther {
				t.Fatalf("status: got %d want 303", w.Code)
			}

			loc := w.Header().Get("Location")
			parsed, err := url.Parse(loc)
			if err != nil {
				t.Fatalf("parse Location %q: %v", loc, err)
			}
			if parsed.Path != "/m/2fa/verify" {
				t.Fatalf("Location path: got %q want /m/2fa/verify", parsed.Path)
			}
			got := parsed.Query().Get("return")
			if got != tc.raw {
				t.Errorf("return round-trip: got %q want %q (raw Location: %q)", got, tc.raw, loc)
			}
		})
	}
}

func TestLoginHandler_POST_DropsUnsafeNextOnRedirect(t *testing.T) {
	store := newFakeSessionStore()
	login := &fakeMasterLogin{result: iam.Session{UserID: uuid.New()}}
	h := newLoginHandlerWith(t, login.Login, store)

	form := url.Values{}
	form.Set("email", "ops@example.com")
	form.Set("password", "x")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/m/login?next=https://evil.com", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(w, r)

	loc := w.Header().Get("Location")
	if strings.Contains(loc, "evil.com") {
		t.Errorf("unsafe next= survived to redirect: %q", loc)
	}
	if loc != "/m/2fa/verify" {
		t.Errorf("Location: got %q want /m/2fa/verify", loc)
	}
}

// ---------------------------------------------------------------------------
// POST error paths
// ---------------------------------------------------------------------------

func TestLoginHandler_POST_BadForm_Returns400(t *testing.T) {
	store := newFakeSessionStore()
	h := newLoginHandlerWith(t, (&fakeMasterLogin{}).Login, store)
	// A request body with content-type form-urlencoded + invalid percent
	// escape forces ParseForm to error.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/m/login", strings.NewReader("%"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

func TestLoginHandler_POST_InvalidCredentials_RendersGenericError(t *testing.T) {
	store := newFakeSessionStore()
	login := &fakeMasterLogin{err: iam.ErrInvalidCredentials}
	h := newLoginHandlerWith(t, login.Login, store)

	form := url.Values{}
	form.Set("email", "ops@example.com")
	form.Set("password", "wrong")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/m/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "credenciais inválidas") {
		t.Errorf("body missing generic error: %s", w.Body.String())
	}
	if store.createCalls != 0 {
		t.Errorf("master session created on credential failure: %d", store.createCalls)
	}
	cookies := w.Result().Cookies()
	for _, c := range cookies {
		if c.Name == sessioncookie.NameMaster && c.MaxAge != -1 {
			t.Errorf("session cookie set on credential failure")
		}
	}
}

func TestLoginHandler_POST_InternalError_RendersGenericErrorAndDoesNotLeak(t *testing.T) {
	store := newFakeSessionStore()
	login := &fakeMasterLogin{err: errors.New("pgx: i/o timeout")}
	h := newLoginHandlerWith(t, login.Login, store)

	form := url.Values{}
	form.Set("email", "ops@example.com")
	form.Set("password", "x")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/m/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401 (anti-enumeration)", w.Code)
	}
	if strings.Contains(w.Body.String(), "i/o timeout") {
		t.Errorf("internal error leaked to body: %s", w.Body.String())
	}
}

// AccountLockedError must surface 429 + Retry-After via the shared
// translator (acceptance criterion #4 — reuse, do not duplicate).
func TestLoginHandler_POST_AccountLocked_Renders429WithRetryAfter(t *testing.T) {
	store := newFakeSessionStore()
	until := time.Now().Add(7 * time.Second)
	login := &fakeMasterLogin{err: &iam.AccountLockedError{Until: until}}
	h := newLoginHandlerWith(t, login.Login, store)

	form := url.Values{}
	form.Set("email", "ops@example.com")
	form.Set("password", "x")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/m/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status: got %d want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Errorf("missing Retry-After")
	}
}

func TestLoginHandler_POST_StoreCreateFailure_Returns500(t *testing.T) {
	store := newFakeSessionStore()
	store.createErr = errors.New("pgx: deadlock")
	login := &fakeMasterLogin{result: iam.Session{UserID: uuid.New()}}
	h := newLoginHandlerWith(t, login.Login, store)

	form := url.Values{}
	form.Set("email", "ops@example.com")
	form.Set("password", "x")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/m/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", w.Code)
	}
}
