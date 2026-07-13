package usermfa

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// pendingHandler builds a handler wired for the mid-login pending path only:
// no TenantSession resolver, so resolveSetupActor always falls through to the
// __Host-mfa-pending predicate. enroller counts Enroll calls — the load-bearing
// assertion for the silent-rotation guard.
func pendingHandler(t *testing.T, deps *testDeps, enroller *countingEnroller) *Handler {
	t.Helper()
	cfg := deps.config()
	cfg.Enroller = enroller
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

// pendingSetupRequest builds a /admin/2fa/setup request carrying the tenant
// scope + the __Host-mfa-pending cookie. body is "" for GET.
func pendingSetupRequest(method string, tenantID, pendingID uuid.UUID, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/admin/2fa/setup", nil)
	} else {
		r = httptest.NewRequest(method, "/admin/2fa/setup", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID, Name: "Acme", Host: "acme.test"}))
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: pendingID.String()})
	return r
}

func addEnrolledPending(deps *testDeps) (user, tenant, id uuid.UUID) {
	user, tenant, id = uuid.New(), uuid.New(), uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: tenant, ExpiresAt: deps.clock.Now().Add(5 * time.Minute), NextPath: "/x"})
	deps.labels.set(user, "admin@acme.test")
	deps.enrollment.mark(user, true)
	return user, tenant, id
}

// SIN-67210 F1 (core regression) — the password-only (pending) path must NOT
// silently rotate the secret of an ALREADY-enrolled account on a bare GET.
// This is the exact staging repro: a session holding only __Host-mfa-pending
// hits GET /admin/2fa/setup and, on the pre-fix handler, Enroll ran and a
// brand-new TOTP secret was committed — letting a stolen password alone
// defeat 2FA. Against the fixed handler Enroll is never reached; the styled
// recovery-gated re-enrol page renders instead.
func TestSetupPendingAlreadyEnrolledGETDoesNotRotate(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	_, tenant, id := addEnrolledPending(deps)
	enroller := &countingEnroller{result: sampleEnrollResult()}
	h := pendingHandler(t, deps, enroller)

	w := httptest.NewRecorder()
	h.Setup(w, pendingSetupRequest(http.MethodGet, tenant, id, ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", w.Code)
	}
	if enroller.count() != 0 {
		t.Fatalf("CRITICAL (F1): Enroll must NOT run for a password-only already-enrolled GET (silent rotation), got %d calls", enroller.count())
	}
	body := w.Body.String()
	if strings.Contains(body, "otpauth://totp") {
		t.Fatalf("F1: a new secret QR must NOT be emitted on the pending GET gate, got:\n%s", body)
	}
	if !strings.Contains(body, "Reconfigurar verificação em duas etapas") {
		t.Fatalf("expected recovery-gated re-enrol page, got:\n%s", body)
	}
	if !strings.Contains(body, "código de recuperação") {
		t.Fatalf("re-enrol gate must mention recovery code, got:\n%s", body)
	}
}

// SIN-67210 F1 — a POST with only the password session (pending) and a bogus
// recovery code must NOT rotate; the gate re-renders with 401 and an audit
// row records the attempt.
func TestSetupPendingAlreadyEnrolledPOSTInvalidRecoveryNoRotate(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	_, tenant, id := addEnrolledPending(deps)
	deps.consumer.accept = "GOOD-RECOVERY" // submitted code differs → invalid
	enroller := &countingEnroller{result: sampleEnrollResult()}
	h := pendingHandler(t, deps, enroller)

	w := httptest.NewRecorder()
	h.Setup(w, pendingSetupRequest(http.MethodPost, tenant, id, "code=WRONG-CODE1"))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401 got %d", w.Code)
	}
	if enroller.count() != 0 {
		t.Fatalf("CRITICAL (F1): Enroll must NOT run on an invalid recovery code, got %d calls", enroller.count())
	}
	if !deps.consumer.called {
		t.Fatalf("expected ConsumeRecovery to be attempted for a non-numeric code")
	}
	if deps.audit.events != 1 {
		t.Fatalf("audit rows: want 1 got %d", deps.audit.events)
	}
	if reason := deps.audit.lastReason(); reason != "stepup_invalid_code" {
		t.Fatalf("audit reason: want stepup_invalid_code got %q", reason)
	}
}

// SIN-67210 — a legitimate re-enrol on the pending path with a VALID recovery
// code rotates the secret (Enroll once) and renders the fresh QR. This is the
// lost-authenticator escape (F2 destination) working end-to-end.
func TestSetupPendingAlreadyEnrolledPOSTValidRecoveryRotates(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	user, tenant, id := addEnrolledPending(deps)
	deps.consumer.accept = "GOOD-RECOVERY"
	enroller := &countingEnroller{result: sampleEnrollResult()}
	h := pendingHandler(t, deps, enroller)

	w := httptest.NewRecorder()
	h.Setup(w, pendingSetupRequest(http.MethodPost, tenant, id, "code=GOOD-RECOVERY"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", w.Code)
	}
	if enroller.count() != 1 {
		t.Fatalf("valid recovery re-enrol must rotate once, got %d", enroller.count())
	}
	if !deps.consumer.called {
		t.Fatalf("expected ConsumeRecovery to be called (single-use burn of the code)")
	}
	if body := w.Body.String(); !strings.Contains(body, "otpauth://totp") {
		t.Fatalf("expected fresh QR after valid recovery re-enrol, got:\n%s", body)
	}
	_ = user
}

// SIN-67210 — a current TOTP is also accepted as proof on the pending re-enrol
// gate (the user still has the authenticator but wants to rotate).
func TestSetupPendingAlreadyEnrolledPOSTValidTOTPRotates(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	_, tenant, id := addEnrolledPending(deps)
	deps.verifier.accept = "123456"
	enroller := &countingEnroller{result: sampleEnrollResult()}
	h := pendingHandler(t, deps, enroller)

	w := httptest.NewRecorder()
	h.Setup(w, pendingSetupRequest(http.MethodPost, tenant, id, "code=123456"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", w.Code)
	}
	if enroller.count() != 1 {
		t.Fatalf("valid TOTP re-enrol must rotate once, got %d", enroller.count())
	}
	if deps.consumer.called {
		t.Fatalf("a 6-digit code must go through Verify, not ConsumeRecovery")
	}
}

// SIN-67210 — an empty submission on the pending gate is a 400 malformed
// request; the secret is never rotated.
func TestSetupPendingAlreadyEnrolledPOSTEmptyCodeNoRotate(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	_, tenant, id := addEnrolledPending(deps)
	enroller := &countingEnroller{result: sampleEnrollResult()}
	h := pendingHandler(t, deps, enroller)

	w := httptest.NewRecorder()
	h.Setup(w, pendingSetupRequest(http.MethodPost, tenant, id, "code="))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400 got %d", w.Code)
	}
	if enroller.count() != 0 {
		t.Fatalf("Enroll must NOT run on an empty code, got %d", enroller.count())
	}
}

// SIN-67210 — brute-forcing recovery codes on the pending gate trips the same
// lockout the verify surface uses: the threshold attempt returns 429 +
// Retry-After, the secret is NEVER rotated, and every attempt is audited.
func TestSetupPendingAlreadyEnrolledRecoveryBruteForceLocksOut(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	user, tenant, id := addEnrolledPending(deps)
	deps.consumer.accept = "GOOD-RECOVERY" // every submitted "BAD-CODE-XX" is invalid
	enroller := &countingEnroller{result: sampleEnrollResult()}
	h := pendingHandler(t, deps, enroller)

	for i := 1; i < 5; i++ {
		w := httptest.NewRecorder()
		h.Setup(w, pendingSetupRequest(http.MethodPost, tenant, id, "code=BAD-CODE-XX"))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status want 401 got %d", i, w.Code)
		}
		if deps.failures.count(user) != i {
			t.Fatalf("attempt %d: failure count want %d got %d", i, i, deps.failures.count(user))
		}
	}

	w := httptest.NewRecorder()
	h.Setup(w, pendingSetupRequest(http.MethodPost, tenant, id, "code=BAD-CODE-XX"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("threshold attempt: status want 429 got %d", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra != "900" {
		t.Fatalf("Retry-After: want 900 got %q", ra)
	}
	if enroller.count() != 0 {
		t.Fatalf("CRITICAL: secret must NOT rotate under brute-force, Enroll calls=%d", enroller.count())
	}
	if reason := deps.audit.lastReason(); reason != "lockout_stepup_invalid_code" {
		t.Fatalf("lockout audit reason: want lockout_stepup_invalid_code got %q", reason)
	}
}

// SIN-67210 — a NOT-yet-enrolled pending user still enrols directly (the
// legitimate forced-enrolment path is unchanged by the F1 fix).
func TestSetupPendingNotEnrolledStillEnrolls(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	user, tenant, id := uuid.New(), uuid.New(), uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: tenant, ExpiresAt: deps.clock.Now().Add(5 * time.Minute), NextPath: "/x"})
	deps.labels.set(user, "admin@acme.test")
	deps.enrollment.mark(user, false)
	enroller := &countingEnroller{result: sampleEnrollResult()}
	h := pendingHandler(t, deps, enroller)

	w := httptest.NewRecorder()
	h.Setup(w, pendingSetupRequest(http.MethodGet, tenant, id, ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", w.Code)
	}
	if enroller.count() != 1 {
		t.Fatalf("first-time pending enrolment must run Enroll once, got %d", enroller.count())
	}
	if body := w.Body.String(); !strings.Contains(body, "otpauth://totp") {
		t.Fatalf("expected enrolment QR body, got:\n%s", body)
	}
}

// SIN-67210 F2 — the /verify page exposes a recovery affordance linking to the
// recovery-code-gated re-enrol flow so a gerente who lost the authenticator is
// not dead-ended.
func TestVerifyPageExposesRecoveryAffordance(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	user, tenant, id := uuid.New(), uuid.New(), uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: tenant, ExpiresAt: deps.clock.Now().Add(5 * time.Minute), NextPath: "/x"})
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
	body := w.Body.String()
	if !strings.Contains(body, `href="/admin/2fa/setup"`) {
		t.Fatalf("verify page must link to /admin/2fa/setup for recovery, got:\n%s", body)
	}
	if !strings.Contains(body, "verify-recovery-link") {
		t.Fatalf("verify page must carry the recovery affordance testid, got:\n%s", body)
	}
}
