package loginhandler_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/loginhandler"
	"github.com/pericles-luz/crm/internal/iam"
)

// loginStub records the most recent invocation and returns the
// caller-supplied (Session, error) for assertion.
type loginStub struct {
	calls    int
	gotHost  string
	gotEmail string
	gotPwd   string
	gotIP    net.IP
	gotUA    string
	gotRoute string
	out      iam.Session
	err      error
}

func (l *loginStub) Login(_ context.Context, host, email, password string, ip net.IP, ua, route string) (iam.Session, error) {
	l.calls++
	l.gotHost = host
	l.gotEmail = email
	l.gotPwd = password
	l.gotIP = ip
	l.gotUA = ua
	l.gotRoute = route
	return l.out, l.err
}

func newRequest(method, body string) *http.Request {
	req := httptest.NewRequest(method, "https://acme.crm.local/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "test-agent/1.0")
	req.RemoteAddr = "203.0.113.5:51000"
	return req
}

func TestHandler_PostSuccess_Returns200WithFragment(t *testing.T) {
	t.Parallel()
	stub := &loginStub{out: iam.Session{ID: uuid.New(), TenantID: uuid.New(), UserID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now()}}
	h := loginhandler.New(stub.Login)
	req := newRequest(http.MethodPost, "email=alice%40acme.test&password=hunter2")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if stub.calls != 1 {
		t.Fatalf("login calls = %d, want 1", stub.calls)
	}
	if stub.gotEmail != "alice@acme.test" {
		t.Fatalf("email = %q, want %q", stub.gotEmail, "alice@acme.test")
	}
	if stub.gotPwd != "hunter2" {
		t.Fatalf("password = %q, want %q", stub.gotPwd, "hunter2")
	}
	if stub.gotHost != "acme.crm.local" {
		t.Fatalf("host = %q, want %q", stub.gotHost, "acme.crm.local")
	}
	if !stub.gotIP.Equal(net.ParseIP("203.0.113.5")) {
		t.Fatalf("ip = %v, want 203.0.113.5", stub.gotIP)
	}
	if stub.gotUA != "test-agent/1.0" {
		t.Fatalf("ua = %q, want test-agent/1.0", stub.gotUA)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html…", ct)
	}
	if !strings.Contains(rec.Body.String(), "signed in") {
		t.Fatalf("body = %q, want fragment containing 'signed in'", rec.Body.String())
	}
}

func TestHandler_AccountLockedError_Returns429WithRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	until := now.Add(425 * time.Second) // ~7m05s — non-round to exercise the ceiling
	lockedErr := (&iam.AccountLockedError{Until: until}).WithClock(func() time.Time { return now })
	stub := &loginStub{err: lockedErr}
	h := loginhandler.New(stub.Login)
	req := newRequest(http.MethodPost, "email=alice%40acme.test&password=WRONG")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	got := rec.Header().Get("Retry-After")
	if got == "" {
		t.Fatal("Retry-After header missing")
	}
	gotSecs, err := strconv.ParseInt(got, 10, 64)
	if err != nil {
		t.Fatalf("Retry-After = %q is not a delta-seconds integer", got)
	}
	if gotSecs != 425 {
		t.Fatalf("Retry-After = %d, want 425 (Until - now)", gotSecs)
	}
	if !strings.Contains(rec.Body.String(), "bloqueada") {
		t.Fatalf("body = %q, want fragment with 'bloqueada'", rec.Body.String())
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("X-Content-Type-Options nosniff missing on lockout response")
	}
}

func TestHandler_AccountLockedError_RoundsUpSubSecondRemainder(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	until := now.Add(2*time.Second + 250*time.Millisecond) // 2.25s
	lockedErr := (&iam.AccountLockedError{Until: until}).WithClock(func() time.Time { return now })
	stub := &loginStub{err: lockedErr}
	h := loginhandler.New(stub.Login)
	req := newRequest(http.MethodPost, "email=a%40b.test&password=x")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	got := rec.Header().Get("Retry-After")
	if got != "3" {
		t.Fatalf("Retry-After = %q, want 3 (math.Ceil(2.25))", got)
	}
}

func TestHandler_AccountLockedError_PastUntilStillReturns1(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	until := now.Add(-time.Hour)
	lockedErr := (&iam.AccountLockedError{Until: until}).WithClock(func() time.Time { return now })
	stub := &loginStub{err: lockedErr}
	h := loginhandler.New(stub.Login)
	req := newRequest(http.MethodPost, "email=a%40b.test&password=x")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1 (clamp floor)", got)
	}
}

func TestHandler_WrappedAccountLockedError_StillExtracted(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	inner := (&iam.AccountLockedError{Until: now.Add(60 * time.Second)}).WithClock(func() time.Time { return now })
	wrapped := fmt.Errorf("login: %w", inner)
	stub := &loginStub{err: wrapped}
	h := loginhandler.New(stub.Login)
	req := newRequest(http.MethodPost, "email=a%40b.test&password=x")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want 60", got)
	}
}

func TestHandler_InvalidCredentials_Returns401(t *testing.T) {
	t.Parallel()
	stub := &loginStub{err: iam.ErrInvalidCredentials}
	h := loginhandler.New(stub.Login)
	req := newRequest(http.MethodPost, "email=ghost%40acme.test&password=anything")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "" {
		t.Fatal("401 response must not carry Retry-After")
	}
	if !strings.Contains(rec.Body.String(), "credenciais inválidas") {
		t.Fatalf("body = %q, want 'credenciais inválidas'", rec.Body.String())
	}
}

func TestHandler_WrappedInvalidCredentials_Returns401(t *testing.T) {
	t.Parallel()
	stub := &loginStub{err: fmt.Errorf("login: %w", iam.ErrInvalidCredentials)}
	h := loginhandler.New(stub.Login)
	req := newRequest(http.MethodPost, "email=g%40b.test&password=x")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandler_InternalError_Returns500GenericBody(t *testing.T) {
	t.Parallel()
	stub := &loginStub{err: errors.New("postgres: connection refused")}
	var sink bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&sink, nil))
	h := loginhandler.New(stub.Login, loginhandler.WithLogger(logger))
	req := newRequest(http.MethodPost, "email=a%40b.test&password=x")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "postgres") {
		t.Fatalf("response body leaks driver-level error: %q", rec.Body.String())
	}
	if !strings.Contains(sink.String(), "postgres: connection refused") {
		t.Fatalf("logger missed the underlying error message: %q", sink.String())
	}
}

func TestHandler_NonPost_Returns405WithAllowHeader(t *testing.T) {
	t.Parallel()
	stub := &loginStub{}
	h := loginhandler.New(stub.Login)
	req := httptest.NewRequest(http.MethodGet, "https://acme.crm.local/login", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", got)
	}
	if stub.calls != 0 {
		t.Fatalf("login calls = %d, want 0", stub.calls)
	}
}

func TestHandler_RemoteAddrIPv6BracketedParses(t *testing.T) {
	t.Parallel()
	stub := &loginStub{out: iam.Session{ID: uuid.New(), TenantID: uuid.New(), UserID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now()}}
	h := loginhandler.New(stub.Login)
	req := newRequest(http.MethodPost, "email=a%40b.test&password=x")
	req.RemoteAddr = "[2001:db8::1]:51000"
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if !stub.gotIP.Equal(net.ParseIP("2001:db8::1")) {
		t.Fatalf("ipv6 ip = %v, want 2001:db8::1", stub.gotIP)
	}
}

func TestHandler_RemoteAddrEmpty_PassesNilIP(t *testing.T) {
	t.Parallel()
	stub := &loginStub{out: iam.Session{ID: uuid.New(), TenantID: uuid.New(), UserID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now()}}
	h := loginhandler.New(stub.Login)
	req := newRequest(http.MethodPost, "email=a%40b.test&password=x")
	req.RemoteAddr = ""
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if stub.gotIP != nil {
		t.Fatalf("empty RemoteAddr: ip = %v, want nil", stub.gotIP)
	}
}

func TestHandler_RemoteAddrBareIPv4_Parses(t *testing.T) {
	t.Parallel()
	stub := &loginStub{out: iam.Session{ID: uuid.New(), TenantID: uuid.New(), UserID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now()}}
	h := loginhandler.New(stub.Login)
	req := newRequest(http.MethodPost, "email=a%40b.test&password=x")
	req.RemoteAddr = "10.0.0.42"
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if !stub.gotIP.Equal(net.ParseIP("10.0.0.42")) {
		t.Fatalf("bare ipv4: ip = %v, want 10.0.0.42", stub.gotIP)
	}
}

func TestHandler_NilLogin_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New(nil) did not panic")
		}
	}()
	loginhandler.New(nil)
}

func TestHandler_WithLogger_NilFallsBackToDefault(t *testing.T) {
	t.Parallel()
	stub := &loginStub{err: iam.ErrInvalidCredentials}
	// Passing nil logger is a no-op; the option only overrides when non-nil.
	h := loginhandler.New(stub.Login, loginhandler.WithLogger(nil))
	req := newRequest(http.MethodPost, "email=a%40b.test&password=x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestWriteLoginError_DirectInvocation_AccountLocked(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	until := now.Add(120 * time.Second)
	locked := (&iam.AccountLockedError{Until: until}).WithClock(func() time.Time { return now })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	loginhandler.WriteLoginError(rec, req, locked, slog.Default())

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "120" {
		t.Fatalf("Retry-After = %q, want 120", got)
	}
}

func TestWriteLoginError_NilLogger_Tolerated(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	loginhandler.WriteLoginError(rec, req, iam.ErrInvalidCredentials, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandler_BadFormBody_Returns400(t *testing.T) {
	t.Parallel()
	stub := &loginStub{}
	h := loginhandler.New(stub.Login)

	// %ZZ is invalid percent-encoding — ParseForm rejects it.
	req := httptest.NewRequest(http.MethodPost, "https://acme.crm.local/login", strings.NewReader("email=%ZZ"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if stub.calls != 0 {
		t.Fatalf("login calls = %d, want 0", stub.calls)
	}
}

// Sanity: the handler does not retain or reuse the request body
// after returning.
func TestHandler_RequestBodyConsumedExactlyOnce(t *testing.T) {
	t.Parallel()
	stub := &loginStub{out: iam.Session{ID: uuid.New(), TenantID: uuid.New(), UserID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now()}}
	h := loginhandler.New(stub.Login)
	body := "email=a%40b.test&password=x"
	req := newRequest(http.MethodPost, body)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	leftover, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read leftover body: %v", err)
	}
	if len(leftover) != 0 {
		t.Fatalf("body leftover = %q, want empty", string(leftover))
	}
}
