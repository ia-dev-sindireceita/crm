package main

// SIN-65223 (Child B) wire-level tests. These boot the REAL
// httpapi.NewRouter with a MasterDeps produced by buildMasterDeps over a
// masterMFAStack of stub ports (the mastermfa ports are stubbed — NOT the
// postgres adapters, per the issue scope), and exercise the /m/* surface
// end-to-end. They prove:
//
//   - the /m/* group is mounted in a real router boot (parent AC2);
//   - deny-by-default: an unauthenticated /m/2fa/enroll 303s to /m/login
//     (RequireMasterAuth is real, not a passthrough);
//   - master logout appends exactly one SecurityEventLogout row with
//     audience="master" to the SAME audit.SplitLogger the tenant /logout
//     uses (closes SIN-63216 AC #1 + AC #2);
//   - login mints a __Host-sess-master cookie and 303s to /m/2fa/verify;
//   - an authed + MFA-verified operator can mint enrolment codes;
//   - the noop stack leaves /m/* unmounted (404).

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/usermfa"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// slogTestLogger returns a logger that discards output so the wire
// tests stay quiet.
func slogTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---- stubs (mastermfa ports) ----------------------------------------

// recordingSplitLogger captures every WriteSecurity call so the logout
// test can assert the master audience row.
type recordingSplitLogger struct {
	security []audit.SecurityAuditEvent
}

func (r *recordingSplitLogger) WriteSecurity(_ context.Context, e audit.SecurityAuditEvent) error {
	r.security = append(r.security, e)
	return nil
}

func (r *recordingSplitLogger) WriteData(_ context.Context, _ audit.DataAuditEvent) error {
	return nil
}

// stubMasterSessions implements mastermfa.SessionStore. The Get session
// and createID are configurable per test; Delete records the ids it was
// asked to remove.
type stubMasterSessions struct {
	session  mastermfa.Session
	getErr   error
	createID uuid.UUID
	deleted  []uuid.UUID
}

func (s *stubMasterSessions) Create(_ context.Context, userID uuid.UUID, ttl time.Duration) (mastermfa.Session, error) {
	return mastermfa.Session{ID: s.createID, UserID: userID, ExpiresAt: time.Now().Add(ttl)}, nil
}

func (s *stubMasterSessions) Get(_ context.Context, _ uuid.UUID) (mastermfa.Session, error) {
	if s.getErr != nil {
		return mastermfa.Session{}, s.getErr
	}
	return s.session, nil
}

func (s *stubMasterSessions) Delete(_ context.Context, id uuid.UUID) error {
	s.deleted = append(s.deleted, id)
	return nil
}

func (s *stubMasterSessions) MarkVerified(_ context.Context, _ uuid.UUID) (time.Time, error) {
	return time.Now(), nil
}

func (s *stubMasterSessions) Touch(_ context.Context, _ uuid.UUID, _ time.Duration) error {
	return nil
}

func (s *stubMasterSessions) RotateID(_ context.Context, _ uuid.UUID) (mastermfa.Session, error) {
	return s.session, nil
}

type stubEnroller struct{}

func (stubEnroller) Enroll(_ context.Context, _ uuid.UUID, _ string) (mfa.EnrollResult, error) {
	return mfa.EnrollResult{
		OTPAuthURI:    "otpauth://totp/Sindireceita%20Master:op@example.com?secret=ABC",
		SecretEncoded: "ABCDEFGH",
		RecoveryCodes: []string{"aaaa-bbbb", "cccc-dddd"},
	}, nil
}

type stubVerifier struct{}

func (stubVerifier) Verify(_ context.Context, _ uuid.UUID, _ string) error { return nil }

type stubConsumer struct{}

func (stubConsumer) ConsumeRecovery(_ context.Context, _ uuid.UUID, _ string, _ mfa.RequestContext) error {
	return nil
}

type stubRegenerator struct{}

func (stubRegenerator) RegenerateRecovery(_ context.Context, _ uuid.UUID, _ mfa.RequestContext) ([]string, error) {
	return []string{"new1-new1"}, nil
}

type stubDirectory struct{ email string }

func (d stubDirectory) EmailFor(_ context.Context, _ uuid.UUID) (string, error) {
	return d.email, nil
}

// stubEnrollment satisfies mastermfa.EnrollmentReader. seed != nil means
// "enrolled" so RequireMasterMFA lets the operator through.
type stubEnrollment struct{ seed []byte }

func (e stubEnrollment) LoadSeed(_ context.Context, _ uuid.UUID) ([]byte, error) {
	return e.seed, nil
}

type stubMasterLockoutAlerter struct{}

func (stubMasterLockoutAlerter) AlertVerifyLockout(_ context.Context, _ mastermfa.VerifyLockoutDetails) error {
	return nil
}

// newTestStack assembles a fully-populated masterMFAStack of stub ports.
// sessions/enrollment/directory let each test tune the auth + MFA gates.
func newTestStack(sessions *stubMasterSessions, enrollment stubEnrollment, dir stubDirectory) masterMFAStack {
	httpSession := mastermfa.NewHTTPSession(sessions)
	return masterMFAStack{
		Sessions:    sessions,
		HTTPSession: httpSession,
		Enroller:    stubEnroller{},
		Verifier:    stubVerifier{},
		Consumer:    stubConsumer{},
		Regenerator: stubRegenerator{},
		Enrollment:  enrollment,
		Directory:   dir,
		Failures:    usermfa.NewMemoryFailureCounter(0),
		Invalidator: httpSession,
		Alerter:     stubMasterLockoutAlerter{},
		Login: func(_ context.Context, _, _, _ string, _ net.IP, _, _ string) (iam.Session, error) {
			return iam.Session{}, iam.ErrInvalidCredentials
		},
	}
}

// mountMaster boots a real router with the deps under test mounted on
// the Master slot. IAM + TenantResolver are the in-package fakes already
// used by the other cmd/server router tests.
func mountMaster(t *testing.T, deps httpapi.MasterDeps) http.Handler {
	t.Helper()
	return httpapi.NewRouter(httpapi.Deps{
		IAM:            stubIAMService{},
		TenantResolver: &stubMasterTenantResolver{},
		Master:         deps,
	})
}

// ---- tests ----------------------------------------------------------

func TestBuildMasterDeps_NoopStackLeavesGroupUnmounted(t *testing.T) {
	t.Parallel()

	deps := buildMasterDeps(noopMasterMFAStack(), &recordingSplitLogger{}, slogTestLogger())
	if deps.Login != nil {
		t.Fatalf("noop stack must yield zero MasterDeps (Login nil), got non-nil")
	}

	h := mountMaster(t, deps)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/m/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /m/login with noop stack: status = %d, want 404 (group skipped)", rec.Code)
	}
}

func TestBuildMasterDeps_DenyByDefault_EnrollRedirectsToLogin(t *testing.T) {
	t.Parallel()

	stack := newTestStack(&stubMasterSessions{}, stubEnrollment{seed: []byte("seed")}, stubDirectory{email: "op@example.com"})
	deps := buildMasterDeps(stack, &recordingSplitLogger{}, slogTestLogger())
	h := mountMaster(t, deps)

	// No __Host-sess-master cookie → RequireMasterAuth must redirect.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/m/2fa/enroll", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauth GET /m/2fa/enroll: status = %d, want 303 (deny-by-default)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/m/login") {
		t.Fatalf("unauth GET /m/2fa/enroll: Location = %q, want /m/login*", loc)
	}
}

func TestBuildMasterDeps_LoginMintsCookieAndRedirectsToVerify(t *testing.T) {
	t.Parallel()

	createID := uuid.New()
	userID := uuid.New()
	sessions := &stubMasterSessions{createID: createID}
	stack := newTestStack(sessions, stubEnrollment{seed: []byte("seed")}, stubDirectory{email: "op@example.com"})
	// Override Login to succeed for this test.
	stack.Login = func(_ context.Context, _, _, _ string, _ net.IP, _, _ string) (iam.Session, error) {
		return iam.Session{UserID: userID}, nil
	}
	deps := buildMasterDeps(stack, &recordingSplitLogger{}, slogTestLogger())
	h := mountMaster(t, deps)

	form := url.Values{"email": {"op@example.com"}, "password": {"hunter2"}}
	req := httptest.NewRequest(http.MethodPost, "/m/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /m/login: status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/m/2fa/verify" {
		t.Fatalf("POST /m/login: Location = %q, want /m/2fa/verify", loc)
	}
	if sc := rec.Header().Get("Set-Cookie"); !strings.Contains(sc, "__Host-sess-master") {
		t.Fatalf("POST /m/login: Set-Cookie = %q, want __Host-sess-master", sc)
	}
}

func TestBuildMasterDeps_LogoutAppendsMasterAudienceRow(t *testing.T) {
	t.Parallel()

	sessionID := uuid.New()
	userID := uuid.New()
	sessions := &stubMasterSessions{
		session: mastermfa.Session{ID: sessionID, UserID: userID},
	}
	stack := newTestStack(sessions, stubEnrollment{seed: []byte("seed")}, stubDirectory{email: "op@example.com"})
	rec := &recordingSplitLogger{}
	deps := buildMasterDeps(stack, rec, slogTestLogger())
	h := mountMaster(t, deps)

	req := httptest.NewRequest(http.MethodGet, "/m/logout", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-sess-master", Value: sessionID.String()})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("GET /m/logout: status = %d, want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/m/login" {
		t.Fatalf("GET /m/logout: Location = %q, want /m/login", loc)
	}

	// Exactly one SecurityEventLogout row, audience="master", nil tenant.
	var logouts []audit.SecurityAuditEvent
	for _, e := range rec.security {
		if e.Event == audit.SecurityEventLogout {
			logouts = append(logouts, e)
		}
	}
	if len(logouts) != 1 {
		t.Fatalf("logout audit rows = %d, want exactly 1", len(logouts))
	}
	got := logouts[0]
	if got.Target["audience"] != "master" {
		t.Errorf("audit audience = %v, want master", got.Target["audience"])
	}
	if got.TenantID != nil {
		t.Errorf("audit TenantID = %v, want nil (master rows are tenant-less)", got.TenantID)
	}
	if got.ActorUserID != userID {
		t.Errorf("audit ActorUserID = %v, want %v", got.ActorUserID, userID)
	}
}

func TestBuildMasterDeps_AuthedVerifiedOperatorCanEnroll(t *testing.T) {
	t.Parallel()

	sessionID := uuid.New()
	userID := uuid.New()
	verifiedAt := time.Now().Add(-time.Minute)
	sessions := &stubMasterSessions{
		session: mastermfa.Session{ID: sessionID, UserID: userID, MFAVerifiedAt: &verifiedAt, ExpiresAt: time.Now().Add(time.Hour)},
	}
	stack := newTestStack(sessions, stubEnrollment{seed: []byte("seed")}, stubDirectory{email: "op@example.com"})
	deps := buildMasterDeps(stack, &recordingSplitLogger{}, slogTestLogger())
	h := mountMaster(t, deps)

	// POST because the enrol handler is POST-only (it mints fresh codes).
	req := httptest.NewRequest(http.MethodPost, "/m/2fa/enroll", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-sess-master", Value: sessionID.String()})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("authed+verified POST /m/2fa/enroll: status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "aaaa-bbbb") {
		t.Errorf("enrol response did not render the recovery codes")
	}
}
