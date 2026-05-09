package handler_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/handler"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

type fakeIAM struct {
	loginErr     error
	logoutErr    error
	loginSession iam.Session
	loginEmail   string
	loginPwd     string
	loginHost    string
	loginUA      string
	loginIP      net.IP
	loginRoute   string
	logoutCalls  int
	logoutTenant uuid.UUID
	logoutSess   uuid.UUID
}

func (f *fakeIAM) Login(_ context.Context, host, email, password string, ipAddr net.IP, userAgent, route string) (iam.Session, error) {
	f.loginHost = host
	f.loginEmail = email
	f.loginPwd = password
	f.loginIP = ipAddr
	f.loginUA = userAgent
	f.loginRoute = route
	if f.loginErr != nil {
		return iam.Session{}, f.loginErr
	}
	return f.loginSession, nil
}

func (f *fakeIAM) Logout(_ context.Context, tenantID, sessionID uuid.UUID) error {
	f.logoutCalls++
	f.logoutTenant = tenantID
	f.logoutSess = sessionID
	return f.logoutErr
}

func tenantedRequest(t *testing.T, method, target string, body io.Reader, tenant *tenancy.Tenant) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, target, body)
	ctx := tenancy.WithContext(r.Context(), tenant)
	if method == http.MethodPost && body != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return r.WithContext(ctx)
}

func TestHealth_Returns200JSON(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.Health(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q, want application/json", got)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("body=%q does not contain status:ok", rec.Body.String())
	}
}

func TestLoginGet_RendersFormWithNext(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.LoginGet(rec, httptest.NewRequest(http.MethodGet, "/login?next=/hello-tenant", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<form method="POST" action="/login">`) {
		t.Fatalf("body missing login form: %q", body)
	}
	if !strings.Contains(body, `name="next" value="/hello-tenant"`) {
		t.Fatalf("body missing next field: %q", body)
	}
}

func TestLoginGet_SanitizesAbsoluteNext(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.LoginGet(rec, httptest.NewRequest(http.MethodGet, "/login?next=https://attacker.example/", nil))
	body := rec.Body.String()
	if strings.Contains(body, "attacker.example") {
		t.Fatalf("body leaked attacker URL: %q", body)
	}
	if !strings.Contains(body, `value="/hello-tenant"`) {
		t.Fatalf("body missing fallback next: %q", body)
	}
}

func TestSanitizeNext_Cases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "/hello-tenant"},
		{"absolute", "https://attacker.example/", "/hello-tenant"},
		{"protocol-relative", "//attacker.example/", "/hello-tenant"},
		{"hostless", "hello-tenant", "/hello-tenant"},
		{"valid path", "/dashboard", "/dashboard"},
		{"path with query", "/dashboard?x=1", "/dashboard?x=1"},
		{"unparseable", "://", "/hello-tenant"},
	}
	for _, tc := range cases {
		got := handler.SanitizeNext(tc.in)
		if got != tc.want {
			t.Errorf("%s: SanitizeNext(%q)=%q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestLoginPost_Success_SetsCookieAndRedirects(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	userID := uuid.New()
	sessID := uuid.New()
	iamFake := &fakeIAM{loginSession: iam.Session{
		ID:        sessID,
		UserID:    userID,
		TenantID:  tenantID,
		ExpiresAt: time.Now().Add(time.Hour),
	}}

	form := url.Values{}
	form.Set("email", "alice@acme.test")
	form.Set("password", "correct-horse-battery-staple")
	form.Set("next", "/hello-tenant")

	r := tenantedRequest(t, http.MethodPost, "/login", strings.NewReader(form.Encode()), &tenancy.Tenant{ID: tenantID, Name: "acme", Host: "acme.crm.local"})
	r.Host = "acme.crm.local"
	r.RemoteAddr = "203.0.113.7:12345"
	r.Header.Set("User-Agent", "ua-fixture")
	rec := httptest.NewRecorder()

	handler.LoginPost(handler.LoginConfig{IAM: iamFake})(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/hello-tenant" {
		t.Fatalf("Location=%q, want /hello-tenant", loc)
	}
	if iamFake.loginEmail != "alice@acme.test" {
		t.Fatalf("Login email=%q, want alice@acme.test", iamFake.loginEmail)
	}
	if iamFake.loginPwd != "correct-horse-battery-staple" {
		t.Fatalf("Login password not propagated")
	}
	if iamFake.loginHost != "acme.crm.local" {
		t.Fatalf("Login host=%q, want acme.crm.local", iamFake.loginHost)
	}
	if iamFake.loginUA != "ua-fixture" {
		t.Fatalf("Login UA=%q, want ua-fixture", iamFake.loginUA)
	}
	if got := iamFake.loginIP.String(); got != "203.0.113.7" {
		t.Fatalf("Login IP=%q, want 203.0.113.7", got)
	}
	// SIN-62379: r.URL.Path threads through to iam.Service.Login so the
	// master-lockout alert can carry route per ADR 0074 §6.
	if iamFake.loginRoute != "/login" {
		t.Fatalf("Login route=%q, want /login", iamFake.loginRoute)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	// ADR 0073 §D2: tenant cookie name MUST carry the __Host- prefix
	// (sessioncookie.NameTenant). The middleware re-export points at
	// the same string so the symbol stays usable in this test.
	if c.Name != middleware.SessionCookieName {
		t.Fatalf("cookie name=%q, want %s", c.Name, middleware.SessionCookieName)
	}
	if c.Value != sessID.String() {
		t.Fatalf("cookie value=%q, want %s", c.Value, sessID.String())
	}
	if !c.HttpOnly {
		t.Fatal("cookie missing HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie SameSite=%v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Fatalf("cookie Path=%q, want /", c.Path)
	}
	// ADR 0073 §D2: Secure is non-negotiable. The login handler always
	// writes the cookie via sessioncookie.SetTenant which hard-codes
	// Secure=true; there is no env override.
	if !c.Secure {
		t.Fatal("cookie Secure must be true (ADR 0073 §D2 — __Host- + Secure)")
	}
}

func TestLoginPost_InvalidCredentials_RendersForm401(t *testing.T) {
	t.Parallel()
	iamFake := &fakeIAM{loginErr: iam.ErrInvalidCredentials}
	form := url.Values{}
	form.Set("email", "alice@acme.test")
	form.Set("password", "wrong")
	r := tenantedRequest(t, http.MethodPost, "/login", strings.NewReader(form.Encode()), &tenancy.Tenant{ID: uuid.New()})
	rec := httptest.NewRecorder()
	handler.LoginPost(handler.LoginConfig{IAM: iamFake})(rec, r)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Email ou senha inválidos.") {
		t.Fatalf("body missing generic error: %q", rec.Body.String())
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("got %d cookies on auth failure, want 0", len(cookies))
	}
}

func TestLoginPost_InfraError_500(t *testing.T) {
	t.Parallel()
	iamFake := &fakeIAM{loginErr: errors.New("db down")}
	form := url.Values{}
	form.Set("email", "alice@acme.test")
	form.Set("password", "pw")
	r := tenantedRequest(t, http.MethodPost, "/login", strings.NewReader(form.Encode()), &tenancy.Tenant{ID: uuid.New()})
	rec := httptest.NewRecorder()
	handler.LoginPost(handler.LoginConfig{IAM: iamFake})(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

// TestLoginPost_AccountLocked_ReturnsRetryAfter is the SIN-62217 ↔ SIN-62348
// integration check: when iam.Service.Login returns *iam.AccountLockedError
// the handler must surface 429 + Retry-After (delta-seconds, rounded up).
// The translation is delegated to loginhandler.WriteLoginError so the
// fragment body and header arithmetic stay in one place across handlers.
func TestLoginPost_AccountLocked_ReturnsRetryAfter(t *testing.T) {
	t.Parallel()
	until := time.Now().Add(75 * time.Second)
	iamFake := &fakeIAM{loginErr: iam.NewAccountLockedError(until)}
	form := url.Values{}
	form.Set("email", "victim@acme.test")
	form.Set("password", "irrelevant")
	r := tenantedRequest(t, http.MethodPost, "/login", strings.NewReader(form.Encode()), &tenancy.Tenant{ID: uuid.New()})
	rec := httptest.NewRecorder()
	handler.LoginPost(handler.LoginConfig{IAM: iamFake})(rec, r)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429", rec.Code)
	}
	got := rec.Header().Get("Retry-After")
	if got == "" {
		t.Fatal("Retry-After header missing")
	}
	// rounded up; for 75s, header must be >= 1 and <= 76 (ceil semantics
	// + 1s minimum) with realistic clock skew on slow CI.
	if got != "75" && got != "76" {
		t.Fatalf("Retry-After=%q, want 75 or 76", got)
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("got %d cookies on lockout, want 0", len(cookies))
	}
}

func TestLoginPost_PanicsOnNilAuthenticator(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil IAM")
		}
	}()
	handler.LoginPost(handler.LoginConfig{})
}

// TestLoginPost_BodyFormInteropWithRateLimitMiddleware is the SIN-62217 G1
// gate test. It proves a middleware that pre-reads r.PostFormValue("email")
// — the shape that internal/http/middleware/ratelimit/FormFieldKey will
// take — does NOT leave the handler with EOF when the handler also reads
// PostFormValue. ParseForm is idempotent and caches its result on the
// request, so handler and middleware see the same value.
func TestLoginPost_BodyFormInteropWithRateLimitMiddleware(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	iamFake := &fakeIAM{loginSession: iam.Session{ID: uuid.New(), UserID: uuid.New(), TenantID: tenantID, ExpiresAt: time.Now().Add(time.Hour)}}

	// preReader simulates RateLimit.FormFieldKey reading the body before
	// the handler. It MUST NOT consume r.Body in a way that prevents the
	// handler from reading PostFormValue.
	var observedByMiddleware string
	preReader := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			observedByMiddleware = r.PostFormValue("email")
			next.ServeHTTP(w, r)
		})
	}

	chain := preReader(handler.LoginPost(handler.LoginConfig{IAM: iamFake}))

	form := url.Values{}
	form.Set("email", "victim@example.com")
	form.Set("password", "correct-horse-battery-staple")
	r := tenantedRequest(t, http.MethodPost, "/login", strings.NewReader(form.Encode()), &tenancy.Tenant{ID: tenantID})
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, r)

	if observedByMiddleware != "victim@example.com" {
		t.Fatalf("middleware saw email=%q, want victim@example.com", observedByMiddleware)
	}
	if iamFake.loginEmail != "victim@example.com" {
		t.Fatalf("handler saw email=%q, want victim@example.com (likely EOF after middleware)", iamFake.loginEmail)
	}
	if iamFake.loginPwd != "correct-horse-battery-staple" {
		t.Fatalf("handler saw password=%q, want correct-horse-battery-staple", iamFake.loginPwd)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
}

func TestLogout_DeletesSessionAndExpiresCookie(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	sessID := uuid.New()
	iamFake := &fakeIAM{}

	r := tenantedRequest(t, http.MethodPost, "/logout", nil, &tenancy.Tenant{ID: tenantID})
	r.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: sessID.String()})
	rec := httptest.NewRecorder()

	handler.Logout(iamFake)(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("Location=%q, want /login", loc)
	}
	if iamFake.logoutCalls != 1 {
		t.Fatalf("Logout calls=%d, want 1", iamFake.logoutCalls)
	}
	if iamFake.logoutTenant != tenantID || iamFake.logoutSess != sessID {
		t.Fatalf("Logout args mismatch: tenant=%v sess=%v", iamFake.logoutTenant, iamFake.logoutSess)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	if cookies[0].MaxAge != -1 || cookies[0].Value != "" {
		t.Fatalf("cookie not expired: %+v", cookies[0])
	}
}

func TestLogout_NoCookie_StillRedirects(t *testing.T) {
	t.Parallel()
	iamFake := &fakeIAM{}
	r := tenantedRequest(t, http.MethodPost, "/logout", nil, &tenancy.Tenant{ID: uuid.New()})
	rec := httptest.NewRecorder()
	handler.Logout(iamFake)(rec, r)
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	if iamFake.logoutCalls != 0 {
		t.Fatalf("Logout calls=%d, want 0 (no cookie -> no iam call)", iamFake.logoutCalls)
	}
}

func TestLogout_BadCookieValue_StillRedirects(t *testing.T) {
	t.Parallel()
	iamFake := &fakeIAM{}
	r := tenantedRequest(t, http.MethodPost, "/logout", nil, &tenancy.Tenant{ID: uuid.New()})
	r.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: "not-a-uuid"})
	rec := httptest.NewRecorder()
	handler.Logout(iamFake)(rec, r)
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	if iamFake.logoutCalls != 0 {
		t.Fatalf("Logout calls=%d, want 0 (bad cookie -> no iam call)", iamFake.logoutCalls)
	}
}

func TestLogout_MissingTenantContext_500(t *testing.T) {
	t.Parallel()
	iamFake := &fakeIAM{}
	r := httptest.NewRequest(http.MethodPost, "/logout", nil)
	rec := httptest.NewRecorder()
	handler.Logout(iamFake)(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestLogout_PanicsOnNilDeleter(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil deleter")
		}
	}()
	handler.Logout(nil)
}

func TestHelloTenant_RendersTenantName(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	userID := uuid.New()
	tenant := &tenancy.Tenant{ID: tenantID, Name: "acme", Host: "acme.crm.local"}
	r := tenantedRequest(t, http.MethodGet, "/hello-tenant", nil, tenant)
	r = r.WithContext(middleware.WithSession(r.Context(), iam.Session{
		ID: uuid.New(), UserID: userID, TenantID: tenantID,
		ExpiresAt: time.Now().Add(time.Hour),
	}))
	rec := httptest.NewRecorder()

	handler.HelloTenant(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "acme") {
		t.Fatalf("body does not contain tenant name: %q", body)
	}
	if !strings.Contains(body, userID.String()) {
		t.Fatalf("body does not contain user id: %q", body)
	}
}

func TestHelloTenant_MissingTenantContext_500(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/hello-tenant", nil)
	handler.HelloTenant(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestHelloTenant_MissingSessionContext_500(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	r := tenantedRequest(t, http.MethodGet, "/hello-tenant", nil, &tenancy.Tenant{ID: tenantID, Name: "acme"})
	rec := httptest.NewRecorder()
	handler.HelloTenant(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}
