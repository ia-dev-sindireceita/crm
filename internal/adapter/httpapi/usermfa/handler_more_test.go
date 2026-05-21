package usermfa

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/mfa"
)

func TestNewHandlerRejectsMissingCollaborators(t *testing.T) {
	t.Parallel()
	if _, err := NewHandler(HandlerConfig{}); err == nil {
		t.Fatalf("expected validation error for empty config")
	}
	if _, err := NewHandler(HandlerConfig{Enroller: &fakeEnroller{}}); err == nil {
		t.Fatalf("expected validation error for partial config")
	}
}

func TestNewHandlerAppliesDefaults(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	cfg := deps.config()
	cfg.LockoutThreshold = 0
	cfg.LockoutWindow = 0
	cfg.FallbackOK = ""
	cfg.LoginPath = ""
	cfg.Logger = nil
	cfg.Now = nil
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if h.cfg.LockoutThreshold != DefaultLockoutThreshold {
		t.Errorf("LockoutThreshold default: got %d", h.cfg.LockoutThreshold)
	}
	if h.cfg.LockoutWindow != DefaultLockoutWindow {
		t.Errorf("LockoutWindow default: got %v", h.cfg.LockoutWindow)
	}
	if h.cfg.FallbackOK != "/hello-tenant" {
		t.Errorf("FallbackOK default: got %q", h.cfg.FallbackOK)
	}
	if h.cfg.Logger == nil {
		t.Errorf("Logger default should be slog.Default()")
	}
	if h.cfg.Now == nil {
		t.Errorf("Now default should be time.Now")
	}
}

func TestVerifyGETRendersForm(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: uuid.New(), TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(5 * time.Minute), NextPath: "/inbox"})
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/2fa/verify", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Verify(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `name="code"`) {
		t.Fatalf("expected verify form in body")
	}
}

func TestVerifyEmptyCodeIsWrongCode(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	user := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(5 * time.Minute)})
	deps.enrollment.mark(user, true)
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/verify", strings.NewReader("code="))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Verify(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401 got %d", w.Code)
	}
	if n := deps.failures.count(id); n != 1 {
		t.Fatalf("failure count: want 1 got %d", n)
	}
}

func TestVerifyRejectsUnknownMethod(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: uuid.New(), TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(5 * time.Minute)})
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodPut, "/admin/2fa/verify", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Verify(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: want 405 got %d", w.Code)
	}
}

func TestSetupRejectsUnknownMethod(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: uuid.New(), TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(5 * time.Minute)})
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodPut, "/admin/2fa/setup", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Setup(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: want 405 got %d", w.Code)
	}
}

func TestRegenerateRejectsGET(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: uuid.New(), TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(5 * time.Minute)})
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/2fa/regenerate", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Regenerate(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: want 405 got %d", w.Code)
	}
}

func TestVerifyEnrollmentCheckErrorReturns500(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	user := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(5 * time.Minute)})
	cfg := deps.config()
	cfg.Enrollment = &errEnrollment{err: errors.New("db down")}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/verify", strings.NewReader("code=123456"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Verify(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500 got %d", w.Code)
	}
}

func TestVerifyInternalErrorReturns500(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	user := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(5 * time.Minute)})
	deps.enrollment.mark(user, true)
	cfg := deps.config()
	cfg.Verifier = &errVerifier{err: errors.New("decrypt fail")}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/verify", strings.NewReader("code=123456"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Verify(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500 got %d", w.Code)
	}
}

func TestVerifyHonoursPOSTNextOverride(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	user := uuid.New()
	tenant := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: tenant, ExpiresAt: deps.clock.Now().Add(5 * time.Minute), NextPath: "/oldnext"})
	deps.enrollment.mark(user, true)
	deps.verifier.accept = "123456"
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/verify", strings.NewReader("code=123456&next=/inbox"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Verify(w, r)
	if loc := w.Header().Get("Location"); loc != "/inbox" {
		t.Fatalf("Location: want /inbox got %q", loc)
	}
}

func TestSafeNextRejectsAbsoluteAndForeignPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw, fallback, want string
	}{
		{"", "/x", "/x"},
		{"/inbox", "/x", "/inbox"},
		{"https://attacker.test/", "/x", "/x"},
		{"//evil/path", "/x", "/x"},
		{"relative/path", "/x", "/x"},
		{":bad", "/x", "/x"},
	}
	for _, tc := range cases {
		got := safeNext(tc.raw, tc.fallback)
		if got != tc.want {
			t.Errorf("safeNext(%q, %q): want %q got %q", tc.raw, tc.fallback, tc.want, got)
		}
	}
}

func TestIsSixDigit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"123456", true},
		{"12345", false},
		{"1234567", false},
		{"abcdef", false},
		{"12345A", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isSixDigit(tc.in); got != tc.want {
			t.Errorf("isSixDigit(%q): want %v got %v", tc.in, tc.want, got)
		}
	}
}

func TestRequirePendingMalformedCookie(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/2fa/verify", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: "not-a-uuid"})
	w := httptest.NewRecorder()
	h.Verify(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401 got %d", w.Code)
	}
	if deps.audit.lastReason() != "malformed_pending_cookie" {
		t.Fatalf("audit reason: want malformed_pending_cookie got %q", deps.audit.lastReason())
	}
}

func TestRequirePendingLookupError(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	// Use a valid UUID that isn't in the store — fakePendings returns "not found".
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/2fa/verify", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: uuid.New().String()})
	w := httptest.NewRecorder()
	h.Verify(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401 got %d", w.Code)
	}
	if deps.audit.lastReason() != "pending_lookup_failed" {
		t.Fatalf("audit reason: want pending_lookup_failed got %q", deps.audit.lastReason())
	}
}

func TestRegenerateInternalErrorReturns500(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: uuid.New(), TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(5 * time.Minute)})
	deps.regenerator.err = errors.New("db fail")
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/regenerate", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Regenerate(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500 got %d", w.Code)
	}
}

func TestSetupEnrollerErrorReturns500(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: uuid.New(), TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(5 * time.Minute)})
	deps.enroller.err = errors.New("seed gen")
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/setup", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Setup(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500 got %d", w.Code)
	}
}

func TestSetupLabelErrorReturns500(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: uuid.New(), TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(5 * time.Minute)})
	cfg := deps.config()
	cfg.Labels = &errLabels{err: errors.New("db")}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/setup", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Setup(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500 got %d", w.Code)
	}
}

func TestMintTenantSessionFailureReturns500(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	user := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(5 * time.Minute)})
	deps.enrollment.mark(user, true)
	deps.verifier.accept = "123456"
	cfg := deps.config()
	cfg.SessionMinter = &errMinter{err: errors.New("session create")}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/verify", strings.NewReader("code=123456"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Verify(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500 got %d", w.Code)
	}
}

// helpers

type errEnrollment struct{ err error }

func (e *errEnrollment) IsEnrolled(_ context.Context, _ uuid.UUID) (bool, error) {
	return false, e.err
}

type errVerifier struct{ err error }

func (v *errVerifier) Verify(_ context.Context, _ uuid.UUID, _ string) error { return v.err }

type errLabels struct{ err error }

func (l *errLabels) LookupLabel(_ context.Context, _, _ uuid.UUID) (string, error) {
	return "", l.err
}

type errMinter struct{ err error }

func (m *errMinter) MintTenantSession(_ context.Context, _, _ uuid.UUID, _, _ string) (iam.Session, error) {
	return iam.Session{}, m.err
}

// Suppress unused-package warning for mfa import retained for test-side fixtures.
var _ = mfa.RecoveryCodeCount
