package usermfa

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// Regression coverage for SIN-63589: when the stored seed ciphertext is
// unreadable under the current SeedCipher key (the verifier surfaces
// mfa.ErrSeedCipherDecode), the handler must NOT respond 500. It must
// flip the user_mfa row into reenroll_required and redirect 303 to
// /admin/2fa/setup so the user can re-enrol. The pending cookie stays
// attached so the setup surface keeps the user authenticated.
func TestVerifySeedCipherDecodeRedirectsToSetupAndMarksReenroll(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	user := uuid.New()
	tenant := uuid.New()
	deps.pendings.add(Pending{
		ID:        id,
		UserID:    user,
		TenantID:  tenant,
		ExpiresAt: deps.clock.Now().Add(5 * time.Minute),
		NextPath:  "/inbox",
	})
	deps.enrollment.mark(user, true)
	cfg := deps.config()
	// Wrap the sentinel exactly the way mfa.Service.Verify does so the
	// handler's errors.Is check is exercised end-to-end.
	cfg.Verifier = &errVerifier{err: fmt.Errorf("mfa: Verify: decrypt seed: %w: aesgcm: open: cipher: message authentication failed", mfa.ErrSeedCipherDecode)}

	var logBuf bytes.Buffer
	cfg.Logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/verify", strings.NewReader("code=145710"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Verify(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303 (redirect to setup) got %d body=%q", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/admin/2fa/setup" {
		t.Fatalf("Location: want /admin/2fa/setup got %q", loc)
	}
	if !deps.reenroller.called(user) {
		t.Fatalf("expected MarkReenrollRequired to be called for %s, got calls=%v", user, deps.reenroller.calls)
	}
	if deps.pendings.deleted(id) {
		t.Fatalf("pending row must remain so the user keeps the cookie through /admin/2fa/setup")
	}
	pendingCookieCleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessioncookie.NameTenantPending && c.MaxAge < 0 {
			pendingCookieCleared = true
		}
	}
	if pendingCookieCleared {
		t.Fatalf("__Host-mfa-pending was cleared; the user would lose context arriving at /admin/2fa/setup")
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "stored seed unreadable") {
		t.Fatalf("expected WARN log substring 'stored seed unreadable', got:\n%s", logs)
	}
	if !strings.Contains(logs, "level=WARN") {
		t.Fatalf("expected WARN level (not ERROR) for operator-driven re-enrol signal, got:\n%s", logs)
	}
	if strings.Contains(logs, `"err":"`) || strings.Contains(logs, "err=") && strings.Contains(logs, "aesgcm") {
		// The handler must not leak the cipher's internal error message
		// to logs as part of the WARN — that goes through the sentinel
		// path. (The Verifier still wrapped it for debug, but the WARN
		// itself should be a clean operator signal.)
		// This assertion is conservative: we only flag if both an err
		// key AND the aesgcm substring appear, which together would
		// indicate the cipher error was logged inside the warn line.
		if line := findLogLine(logs, "stored seed unreadable"); strings.Contains(line, "aesgcm") {
			t.Fatalf("WARN line leaks cipher internals: %s", line)
		}
	}
}

// TestVerifySeedCipherDecodeReenrollerFailureReturns500 covers the
// branch where the reenroll-mark write itself fails. We can't safely
// silent-success the user (their next request would still see the
// stale ciphertext) so the handler logs ERROR and returns 500. This
// keeps the bug visible to ops without falling back to the original
// confusing behaviour for the common rotated-key case.
func TestVerifySeedCipherDecodeReenrollerFailureReturns500(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	user := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(5 * time.Minute)})
	deps.enrollment.mark(user, true)
	cfg := deps.config()
	cfg.Verifier = &errVerifier{err: fmt.Errorf("decrypt: %w", mfa.ErrSeedCipherDecode)}
	cfg.Reenroller = &fakeReenroller{err: errors.New("db unreachable")}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/verify", strings.NewReader("code=145710"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Verify(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500 (reenroll write failed) got %d", w.Code)
	}
}

// TestNewHandlerRejectsMissingReenroller is a wireup-bug guard.
func TestNewHandlerRejectsMissingReenroller(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	cfg := deps.config()
	cfg.Reenroller = nil
	if _, err := NewHandler(cfg); err == nil {
		t.Fatalf("expected NewHandler to reject a nil Reenroller")
	}
}

// findLogLine returns the first newline-terminated line in s that
// contains needle, or "" if none does. Helper for assertions on the
// slog TextHandler stream.
func findLogLine(s, needle string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
