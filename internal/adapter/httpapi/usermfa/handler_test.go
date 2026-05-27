package usermfa

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/mfa"
)

func TestRequirePendingMissingCookieAuditsAndDenies(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin/2fa/verify", nil)
	h.Verify(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401 got %d", w.Code)
	}
	if got := deps.audit.lastReason(); got != "missing_pending_cookie" {
		t.Fatalf("audit reason: want missing_pending_cookie got %q", got)
	}
}

func TestRequirePendingExpiredPurgesAndDenies(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := deps.pendings.add(Pending{ID: uuid.New(), UserID: uuid.New(), TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(-time.Second), NextPath: "/x"})
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/2fa/verify", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Verify(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401 got %d", w.Code)
	}
	if !deps.pendings.deleted(id) {
		t.Fatalf("expected expired pending to be purged")
	}
}

func TestVerifyTOTPSuccessMintsSessionAndRedirects(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	user := uuid.New()
	tenant := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: tenant, ExpiresAt: deps.clock.Now().Add(5 * time.Minute), NextPath: "/inbox"})
	deps.enrollment.mark(user, true)
	deps.verifier.accept = "123456"
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	body := url.Values{"code": []string{"123456"}}
	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/verify", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Verify(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303 got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/inbox" {
		t.Fatalf("Location: want /inbox got %q", loc)
	}
	if !deps.pendings.deleted(id) {
		t.Fatalf("expected pending to be deleted on success")
	}
	hasTenant := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessioncookie.NameTenant && c.Value != "" {
			hasTenant = true
		}
	}
	if !hasTenant {
		t.Fatalf("expected __Host-sess-tenant cookie to be set")
	}
}

func TestVerifyWrongCodeIncrementsCounter(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	user := uuid.New()
	tenant := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: tenant, ExpiresAt: deps.clock.Now().Add(5 * time.Minute), NextPath: "/x"})
	deps.enrollment.mark(user, true)
	deps.verifier.accept = "999999"
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	body := url.Values{"code": []string{"123456"}}
	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/verify", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Verify(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401 got %d", w.Code)
	}
	if n := deps.failures.count(id); n != 1 {
		t.Fatalf("failure count after one wrong code: want 1 got %d", n)
	}
}

func TestVerifyLockoutAfterFiveWrongCodes(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	user := uuid.New()
	tenant := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: tenant, ExpiresAt: deps.clock.Now().Add(15 * time.Minute), NextPath: "/x"})
	deps.enrollment.mark(user, true)
	deps.verifier.accept = "999999"
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	var lastStatus int
	for attempt := 1; attempt <= 5; attempt++ {
		body := url.Values{"code": []string{"123456"}}
		r := httptest.NewRequest(http.MethodPost, "/admin/2fa/verify", strings.NewReader(body.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
		w := httptest.NewRecorder()
		h.Verify(w, r)
		lastStatus = w.Code
		if attempt < 5 && w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401 got %d", attempt, w.Code)
		}
		if attempt == 5 {
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("attempt 5: want 429 got %d", w.Code)
			}
			if ra := w.Header().Get("Retry-After"); ra == "" {
				t.Fatalf("attempt 5: missing Retry-After header")
			}
		}
	}
	if lastStatus != http.StatusTooManyRequests {
		t.Fatalf("expected final status 429, got %d", lastStatus)
	}
	if !deps.pendings.deleted(id) {
		t.Fatalf("expected pending to be deleted on lockout")
	}
	if reason := deps.audit.lastReason(); !strings.HasPrefix(reason, "lockout_") {
		t.Fatalf("audit reason: want lockout_ prefix got %q", reason)
	}
}

func TestVerifyRecoveryCodeSuccess(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	user := uuid.New()
	tenant := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: tenant, ExpiresAt: deps.clock.Now().Add(5 * time.Minute), NextPath: "/x"})
	deps.enrollment.mark(user, true)
	deps.consumer.accept = "ABCDE-12345"
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	body := url.Values{"code": []string{"ABCDE-12345"}}
	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/verify", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Verify(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303 got %d", w.Code)
	}
	if !deps.consumer.called {
		t.Fatalf("expected ConsumeRecovery to be called")
	}
}

func TestVerifyNotEnrolledRedirectsToSetup(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	user := uuid.New()
	tenant := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: tenant, ExpiresAt: deps.clock.Now().Add(5 * time.Minute), NextPath: "/x"})
	deps.enrollment.mark(user, false)
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	body := url.Values{"code": []string{"123456"}}
	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/verify", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Verify(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303 got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/admin/2fa/setup" {
		t.Fatalf("Location: want /admin/2fa/setup got %q", loc)
	}
}

func TestSetupRendersQRAndCodes(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	user := uuid.New()
	tenant := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: tenant, ExpiresAt: deps.clock.Now().Add(5 * time.Minute), NextPath: "/x"})
	deps.labels.set(user, "admin@acme.test")
	deps.enroller.result = mfa.EnrollResult{
		OTPAuthURI:    "otpauth://totp/Sindireceita:admin@acme.test?secret=ABC",
		SecretEncoded: "ABCDEFGHJKLMNPQRSTUVWXYZ234567",
		RecoveryCodes: []string{"AAAAAAAAAA", "BBBBBBBBBB"},
	}
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/setup", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Setup(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"ABCDEFGHJKLMNPQRSTUVWXYZ234567", "otpauth://totp", "AAAAA-AAAAA", "BBBBB-BBBBB"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected body to contain %q, got:\n%s", want, body)
		}
	}
}

func TestRegenerateMintsFreshCodes(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	id := uuid.New()
	user := uuid.New()
	tenant := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: user, TenantID: tenant, ExpiresAt: deps.clock.Now().Add(5 * time.Minute), NextPath: "/x"})
	deps.regenerator.codes = []string{"CCCCCCCCCC", "DDDDDDDDDD"}
	h, err := NewHandler(deps.config())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/2fa/regenerate", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Regenerate(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"CCCCC-CCCCC", "DDDDD-DDDDD"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected body to contain %q, got:\n%s", want, body)
		}
	}
}

// ---- fakes ----

type testDeps struct {
	pendings    *fakePendings
	enrollment  *fakeEnrollment
	reenroller  *fakeReenroller
	enroller    *fakeEnroller
	verifier    *fakeVerifier
	consumer    *fakeConsumer
	regenerator *fakeRegenerator
	failures    *fakeFailures
	audit       *fakeAudit
	labels      *fakeLabels
	minter      *fakeMinter
	clock       *fakeClock
}

func newTestDeps() *testDeps {
	clock := &fakeClock{t: time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)}
	return &testDeps{
		pendings:    newFakePendings(),
		enrollment:  newFakeEnrollment(),
		reenroller:  &fakeReenroller{},
		enroller:    &fakeEnroller{},
		verifier:    &fakeVerifier{},
		consumer:    &fakeConsumer{},
		regenerator: &fakeRegenerator{},
		failures:    &fakeFailures{counts: map[uuid.UUID]int{}},
		audit:       &fakeAudit{},
		labels:      newFakeLabels(),
		minter:      &fakeMinter{},
		clock:       clock,
	}
}

func (d *testDeps) config() HandlerConfig {
	return HandlerConfig{
		Enroller:         d.enroller,
		Verifier:         d.verifier,
		Consumer:         d.consumer,
		Regenerator:      d.regenerator,
		Pendings:         d.pendings,
		Enrollment:       d.enrollment,
		Reenroller:       d.reenroller,
		SessionMinter:    d.minter,
		Failures:         d.failures,
		Audit:            d.audit,
		Labels:           d.labels,
		LockoutThreshold: 5,
		LockoutWindow:    15 * time.Minute,
		Now:              d.clock.Now,
	}
}

type fakePendings struct {
	mu      sync.Mutex
	rows    map[uuid.UUID]Pending
	deletes map[uuid.UUID]bool
}

func newFakePendings() *fakePendings {
	return &fakePendings{rows: map[uuid.UUID]Pending{}, deletes: map[uuid.UUID]bool{}}
}

func (p *fakePendings) add(row Pending) uuid.UUID {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rows[row.ID] = row
	return row.ID
}

func (p *fakePendings) Get(_ context.Context, id uuid.UUID) (Pending, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	row, ok := p.rows[id]
	if !ok {
		return Pending{}, errors.New("not found")
	}
	return row, nil
}

func (p *fakePendings) Delete(_ context.Context, id uuid.UUID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.rows, id)
	p.deletes[id] = true
	return nil
}

func (p *fakePendings) deleted(id uuid.UUID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deletes[id]
}

type fakeEnrollment struct {
	mu     sync.Mutex
	status map[uuid.UUID]bool
}

func newFakeEnrollment() *fakeEnrollment {
	return &fakeEnrollment{status: map[uuid.UUID]bool{}}
}
func (f *fakeEnrollment) mark(u uuid.UUID, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[u] = ok
}
func (f *fakeEnrollment) IsEnrolled(_ context.Context, u uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[u], nil
}

type fakeReenroller struct {
	mu    sync.Mutex
	calls []uuid.UUID
	err   error
}

func (f *fakeReenroller) MarkReenrollRequired(_ context.Context, userID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	return f.err
}

func (f *fakeReenroller) called(u uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.calls {
		if id == u {
			return true
		}
	}
	return false
}

type fakeEnroller struct {
	result mfa.EnrollResult
	err    error
}

func (f *fakeEnroller) Enroll(_ context.Context, _ uuid.UUID, _ string) (mfa.EnrollResult, error) {
	if f.err != nil {
		return mfa.EnrollResult{}, f.err
	}
	return f.result, nil
}

type fakeVerifier struct {
	accept string
}

func (f *fakeVerifier) Verify(_ context.Context, _ uuid.UUID, code string) error {
	if code == f.accept {
		return nil
	}
	return mfa.ErrInvalidCode
}

type fakeConsumer struct {
	accept string
	called bool
}

func (f *fakeConsumer) ConsumeRecovery(_ context.Context, _ uuid.UUID, code string, _ mfa.RequestContext) error {
	f.called = true
	if code == f.accept {
		return nil
	}
	return mfa.ErrInvalidCode
}

type fakeRegenerator struct {
	codes []string
	err   error
}

func (f *fakeRegenerator) RegenerateRecovery(_ context.Context, _ uuid.UUID, _ mfa.RequestContext) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.codes, nil
}

type fakeFailures struct {
	mu     sync.Mutex
	counts map[uuid.UUID]int
}

func (f *fakeFailures) Increment(_ context.Context, id uuid.UUID) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[id]++
	return f.counts[id], nil
}
func (f *fakeFailures) Reset(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.counts, id)
	return nil
}
func (f *fakeFailures) count(id uuid.UUID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[id]
}

type fakeAudit struct {
	mu     sync.Mutex
	last   string
	events int
}

func (f *fakeAudit) LogMFARequired(_ context.Context, _ uuid.UUID, _, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = reason
	f.events++
	return nil
}
func (f *fakeAudit) lastReason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

type fakeLabels struct {
	mu sync.Mutex
	m  map[uuid.UUID]string
}

func newFakeLabels() *fakeLabels { return &fakeLabels{m: map[uuid.UUID]string{}} }
func (f *fakeLabels) set(u uuid.UUID, label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[u] = label
}
func (f *fakeLabels) LookupLabel(_ context.Context, _, u uuid.UUID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.m[u]; ok {
		return v, nil
	}
	return "user@example.test", nil
}

type fakeMinter struct{}

func (f *fakeMinter) MintTenantSession(_ context.Context, tenantID, userID uuid.UUID, _, _ string) (iam.Session, error) {
	id := uuid.New()
	return iam.Session{ID: id, UserID: userID, TenantID: tenantID, CSRFToken: "csrf-" + id.String()}, nil
}
