package usermfa

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam/mfa"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// fakeTenantSession is the full-session predicate double. It returns a
// fixed actor or err regardless of input so a test controls exactly which
// branch of resolveSetupActor fires.
type fakeTenantSession struct {
	actor TenantSessionActor
	err   error
}

func (f fakeTenantSession) ResolveTenantSession(_ context.Context, _, _ uuid.UUID) (TenantSessionActor, error) {
	return f.actor, f.err
}

// countingEnroller proves whether Enroll was reached — the load-bearing
// assertion for the silent-rotation guard (AC #2).
type countingEnroller struct {
	mu     sync.Mutex
	calls  int
	result mfa.EnrollResult
}

func (e *countingEnroller) Enroll(_ context.Context, _ uuid.UUID, _ string) (mfa.EnrollResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	return e.result, nil
}

func (e *countingEnroller) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func sampleEnrollResult() mfa.EnrollResult {
	return mfa.EnrollResult{
		OTPAuthURI:    "otpauth://totp/Sindireceita:agent@acme.test?secret=ABC",
		SecretEncoded: "ABCDEFGHJKLMNPQRSTUVWXYZ234567",
		RecoveryCodes: []string{"AAAAAAAAAA", "BBBBBBBBBB"},
	}
}

// fullSessionRequest builds a request that carries a host-resolved tenant
// scope (so tenancy.FromContext succeeds in resolveFullSession) plus the
// __Host-sess-tenant cookie. body is nil for GET; for POST callers pass a
// url-encoded form string.
func fullSessionRequest(method, target string, tenantID, sessionID uuid.UUID, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{
		ID:   tenantID,
		Name: "Acme",
		Host: "acme.test",
	}))
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenant, Value: sessionID.String()})
	return r
}

func newFullSessionHandler(t *testing.T, deps *testDeps, enroller *countingEnroller, session TenantSessionResolver) *Handler {
	t.Helper()
	cfg := deps.config()
	cfg.Enroller = enroller
	cfg.TenantSession = session
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

// AC #1 — a logged-in attendant with no 2FA yet clicks "Configurar 2FA"
// and reaches the enrolment UI (QR + recovery codes), HTTP 200, no raw 401.
func TestSetupFullSessionNotEnrolledRendersEnrollment(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	user, tenant, session := uuid.New(), uuid.New(), uuid.New()
	deps.labels.set(user, "agent@acme.test")
	deps.enrollment.mark(user, false)
	enroller := &countingEnroller{result: sampleEnrollResult()}
	h := newFullSessionHandler(t, deps, enroller, fakeTenantSession{actor: TenantSessionActor{UserID: user, TenantID: tenant}})

	w := httptest.NewRecorder()
	h.Setup(w, fullSessionRequest(http.MethodGet, "/admin/2fa/setup", tenant, session, ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"ABCDEFGHJKLMNPQRSTUVWXYZ234567", "otpauth://totp", "AAAAA-AAAAA"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected enrolment body to contain %q, got:\n%s", want, body)
		}
	}
	if enroller.count() != 1 {
		t.Fatalf("Enroll calls: want 1 got %d", enroller.count())
	}
	if deps.audit.events != 0 {
		t.Fatalf("no audit row expected on a legitimate full-session visit, got %d", deps.audit.events)
	}
}

// AC #2 — an already-enrolled full-session user hitting GET /admin/2fa/setup
// must NOT have the secret rotated; Enroll is never called and the styled
// "já está ativo" page renders.
func TestSetupFullSessionAlreadyEnrolledGETDoesNotRotate(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	user, tenant, session := uuid.New(), uuid.New(), uuid.New()
	deps.enrollment.mark(user, true)
	enroller := &countingEnroller{result: sampleEnrollResult()}
	h := newFullSessionHandler(t, deps, enroller, fakeTenantSession{actor: TenantSessionActor{UserID: user, TenantID: tenant}})

	w := httptest.NewRecorder()
	h.Setup(w, fullSessionRequest(http.MethodGet, "/admin/2fa/setup", tenant, session, ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", w.Code)
	}
	if enroller.count() != 0 {
		t.Fatalf("CRITICAL: Enroll must NOT be called for an already-enrolled GET (silent rotation), got %d calls", enroller.count())
	}
	if body := w.Body.String(); !strings.Contains(body, "2FA já está ativo") {
		t.Fatalf("expected already-active page, got:\n%s", body)
	}
}

// AC #2 (step-up) — re-enrolling an existing secret requires a valid
// current TOTP. A valid code rotates (Enroll called once); the fresh QR
// renders.
func TestSetupFullSessionAlreadyEnrolledPOSTValidStepUpRotates(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	user, tenant, session := uuid.New(), uuid.New(), uuid.New()
	deps.labels.set(user, "agent@acme.test")
	deps.enrollment.mark(user, true)
	deps.verifier.accept = "123456"
	enroller := &countingEnroller{result: sampleEnrollResult()}
	h := newFullSessionHandler(t, deps, enroller, fakeTenantSession{actor: TenantSessionActor{UserID: user, TenantID: tenant}})

	w := httptest.NewRecorder()
	h.Setup(w, fullSessionRequest(http.MethodPost, "/admin/2fa/setup", tenant, session, "code=123456"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", w.Code)
	}
	if enroller.count() != 1 {
		t.Fatalf("Enroll calls after valid step-up: want 1 got %d", enroller.count())
	}
	if body := w.Body.String(); !strings.Contains(body, "otpauth://totp") {
		t.Fatalf("expected fresh QR after step-up, got:\n%s", body)
	}
}

// AC #2 (step-up failure) — an invalid step-up code must NOT rotate the
// secret; the already-active page re-renders with an error and 401.
func TestSetupFullSessionAlreadyEnrolledPOSTInvalidStepUpNoRotate(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	user, tenant, session := uuid.New(), uuid.New(), uuid.New()
	deps.enrollment.mark(user, true)
	deps.verifier.accept = "654321" // submitted code will differ
	enroller := &countingEnroller{result: sampleEnrollResult()}
	h := newFullSessionHandler(t, deps, enroller, fakeTenantSession{actor: TenantSessionActor{UserID: user, TenantID: tenant}})

	w := httptest.NewRecorder()
	h.Setup(w, fullSessionRequest(http.MethodPost, "/admin/2fa/setup", tenant, session, "code=000000"))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401 got %d", w.Code)
	}
	if enroller.count() != 0 {
		t.Fatalf("CRITICAL: Enroll must NOT be called when step-up fails, got %d calls", enroller.count())
	}
	if body := w.Body.String(); !strings.Contains(body, "Código inválido") {
		t.Fatalf("expected step-up error, got:\n%s", body)
	}
}

// AC #2 (non-numeric step-up) — a malformed step-up code is rejected with
// 400 before any verify/enroll.
func TestSetupFullSessionAlreadyEnrolledPOSTBadFormatNoRotate(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	user, tenant, session := uuid.New(), uuid.New(), uuid.New()
	deps.enrollment.mark(user, true)
	enroller := &countingEnroller{result: sampleEnrollResult()}
	h := newFullSessionHandler(t, deps, enroller, fakeTenantSession{actor: TenantSessionActor{UserID: user, TenantID: tenant}})

	w := httptest.NewRecorder()
	h.Setup(w, fullSessionRequest(http.MethodPost, "/admin/2fa/setup", tenant, session, "code=abc"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400 got %d", w.Code)
	}
	if enroller.count() != 0 {
		t.Fatalf("Enroll must not run on a malformed step-up code, got %d", enroller.count())
	}
}

// AC #4 — no session and no pending cookie redirects to the styled login
// page (303), and does NOT emit a raw "2FA required" body or a false
// 2fa_required audit row.
func TestSetupNoSessionNoPendingRedirectsToLoginNoAudit(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	enroller := &countingEnroller{result: sampleEnrollResult()}
	// Resolver wired but reports no session; request carries neither a
	// session cookie nor a pending cookie.
	h := newFullSessionHandler(t, deps, enroller, fakeTenantSession{err: ErrNoTenantSession})

	r := httptest.NewRequest(http.MethodGet, "/admin/2fa/setup", nil)
	r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: uuid.New(), Name: "Acme", Host: "acme.test"}))
	w := httptest.NewRecorder()
	h.Setup(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303 got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/login?next=") {
		t.Fatalf("Location: want /login?next=… got %q", loc)
	}
	if strings.Contains(w.Body.String(), "2FA required") {
		t.Fatalf("must not emit the raw 401 body on a styled redirect")
	}
	if deps.audit.events != 0 {
		t.Fatalf("no 2fa_required audit row expected on the redirect path, got %d", deps.audit.events)
	}
}

// AC #3 — with the resolver wired, a mid-login user (valid pending cookie,
// resolver reports no session) keeps the original enrol-directly behaviour.
func TestSetupMidLoginPendingUnchangedWhenResolverWired(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	user, tenant := uuid.New(), uuid.New()
	id := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: tenant, ExpiresAt: deps.clock.Now().Add(5 * time.Minute), NextPath: "/x"})
	deps.labels.set(user, "agent@acme.test")
	enroller := &countingEnroller{result: sampleEnrollResult()}
	h := newFullSessionHandler(t, deps, enroller, fakeTenantSession{err: ErrNoTenantSession})

	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/setup", nil)
	// A bogus session cookie that the resolver rejects, forcing the
	// fall-through to the pending predicate.
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenant, Value: uuid.New().String()})
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenant, Name: "Acme", Host: "acme.test"}))
	w := httptest.NewRecorder()
	h.Setup(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", w.Code)
	}
	if enroller.count() != 1 {
		t.Fatalf("mid-login pending path must still enrol directly: Enroll calls want 1 got %d", enroller.count())
	}
	if body := w.Body.String(); !strings.Contains(body, "otpauth://totp") {
		t.Fatalf("expected enrolment body on the pending path, got:\n%s", body)
	}
}

// Guard: an enrollment-check failure on the full-session path surfaces a
// 500 and never rotates.
func TestSetupFullSessionEnrollmentCheckErrorReturns500(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	user, tenant, session := uuid.New(), uuid.New(), uuid.New()
	cfg := deps.config()
	cfg.Enrollment = erroringEnrollment{}
	cfg.TenantSession = fakeTenantSession{actor: TenantSessionActor{UserID: user, TenantID: tenant}}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	w := httptest.NewRecorder()
	h.Setup(w, fullSessionRequest(http.MethodGet, "/admin/2fa/setup", tenant, session, ""))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500 got %d", w.Code)
	}
}

type erroringEnrollment struct{}

func (erroringEnrollment) IsEnrolled(_ context.Context, _ uuid.UUID) (bool, error) {
	return false, context.DeadlineExceeded
}
