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
	touchErr error
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
	return s.touchErr
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
// "enrolled" so RequireMasterMFA lets the operator through. loadErr, when
// non-nil, is returned instead of the seed (use mfa.ErrNotEnrolled to
// simulate a first-time operator).
type stubEnrollment struct {
	seed    []byte
	loadErr error
}

func (e stubEnrollment) LoadSeed(_ context.Context, _ uuid.UUID) ([]byte, error) {
	if e.loadErr != nil {
		return nil, e.loadErr
	}
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

// masterWireTestHost is the fake MASTER_CONSOLE_HOST used in wire tests.
// It must match the value passed to buildMasterDeps so the CSRF-6 Origin
// gate (RequireMasterOriginCSRF) and the bridge (RequirePrincipalFromMaster)
// accept requests from the test harness.
const masterWireTestHost = "master.crm.local"

// mountMaster boots a real router with the deps under test mounted on
// the Master slot. IAM + TenantResolver are the in-package fakes already
// used by the other cmd/server router tests. MasterHost must be set so
// the CSRF-6 gate (RequireMasterOriginCSRF) accepts test POSTs; callers
// set matching Host + Origin headers via mReq.
func mountMaster(t *testing.T, deps httpapi.MasterDeps) http.Handler {
	t.Helper()
	return httpapi.NewRouter(httpapi.Deps{
		IAM:            stubIAMService{},
		TenantResolver: &stubMasterTenantResolver{},
		MasterHost:     masterWireTestHost,
		Master:         deps,
	})
}

// mReq builds a request targeted at the master-console surface with the
// Host and Origin headers the CSRF-6 gate requires.
func mReq(method, path string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, path, body)
	r.Host = masterWireTestHost
	r.Header.Set("Origin", "https://"+masterWireTestHost)
	return r
}

// mPost is a convenience wrapper for POST requests.
func mPost(path string, body io.Reader) *http.Request { return mReq(http.MethodPost, path, body) }

// ---- tests ----------------------------------------------------------

func TestBuildMasterDeps_NoopStackLeavesGroupUnmounted(t *testing.T) {
	t.Parallel()

	deps, _ := buildMasterDeps(noopMasterMFAStack(), &recordingSplitLogger{}, slogTestLogger(), "master.crm.local")
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
	deps, _ := buildMasterDeps(stack, &recordingSplitLogger{}, slogTestLogger(), "master.crm.local")
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

// TestBuildMasterDeps_LoginEnrollmentNonNil is the CTO guardrail from
// SIN-65264 (comment 22cda00b): the Login handler MUST have Enrollment
// wired non-nil so a not-yet-enrolled master is routed to /m/2fa/enroll
// instead of getting stuck at /m/2fa/verify with no seed. A nil Enrollment
// silently falls back to always-verify — that is the old broken behaviour.
func TestBuildMasterDeps_LoginEnrollmentNonNil(t *testing.T) {
	t.Parallel()

	stack := newTestStack(&stubMasterSessions{}, stubEnrollment{loadErr: mfa.ErrNotEnrolled}, stubDirectory{email: "op@example.com"})
	stack.Login = func(_ context.Context, _, _, _ string, _ net.IP, _, _ string) (iam.Session, error) {
		return iam.Session{UserID: uuid.New()}, nil
	}
	deps, _ := buildMasterDeps(stack, &recordingSplitLogger{}, slogTestLogger(), masterWireTestHost)
	h := mountMaster(t, deps)

	form := url.Values{"email": {"op@example.com"}, "password": {"hunter2"}}
	req := mPost("/m/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /m/login (not-enrolled): status = %d, want 303", w.Code)
	}
	// Not-enrolled → must route to /m/2fa/enroll, NOT /m/2fa/verify.
	if loc := w.Header().Get("Location"); loc != "/m/2fa/enroll" {
		t.Fatalf("not-enrolled login Location = %q, want /m/2fa/enroll (Enrollment wired non-nil)", loc)
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
	deps, _ := buildMasterDeps(stack, &recordingSplitLogger{}, slogTestLogger(), "master.crm.local")
	h := mountMaster(t, deps)

	form := url.Values{"email": {"op@example.com"}, "password": {"hunter2"}}
	req := mPost("/m/login", strings.NewReader(form.Encode()))
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
	deps, _ := buildMasterDeps(stack, rec, slogTestLogger(), "master.crm.local")
	h := mountMaster(t, deps)

	// SIN-65232: /m/logout is POST-only (forced-logout CSRF fix), so the
	// wire test drives it via POST.
	req := mPost("/m/logout", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-sess-master", Value: sessionID.String()})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /m/logout: status = %d, want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/m/login" {
		t.Fatalf("POST /m/logout: Location = %q, want /m/login", loc)
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

// TestBuildMasterDeps_HardCapHitAuditsAndClearsCookie proves the
// SIN-65232 wireup: a master session past the hard TTL (SessionStore.Touch
// reports ErrSessionHardCap) hitting an /m/* route appends EXACTLY ONE
// master.session.hard_cap_hit row — audience="master", nil tenant — to the
// SAME shared SplitLogger, AND still clears the __Host-sess-master cookie
// and 303s to /m/login. Both controls fire (defense in depth): the audit
// row and the session teardown are independent.
func TestBuildMasterDeps_HardCapHitAuditsAndClearsCookie(t *testing.T) {
	t.Parallel()

	sessionID := uuid.New()
	userID := uuid.New()
	sessions := &stubMasterSessions{
		session:  mastermfa.Session{ID: sessionID, UserID: userID, ExpiresAt: time.Now().Add(time.Hour)},
		touchErr: mastermfa.ErrSessionHardCap,
	}
	stack := newTestStack(sessions, stubEnrollment{seed: []byte("seed")}, stubDirectory{email: "op@example.com"})
	rec := &recordingSplitLogger{}
	deps, _ := buildMasterDeps(stack, rec, slogTestLogger(), "master.crm.local")
	h := mountMaster(t, deps)

	// /m/2fa/verify is the cheapest route behind RequireMasterAuth (auth
	// only, no MFA gate). The valid Get + hard-cap Touch drives the breach
	// path inside the middleware before the handler runs.
	req := httptest.NewRequest(http.MethodGet, "/m/2fa/verify", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-sess-master", Value: sessionID.String()})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	// Defense-in-depth control #1: cookie cleared + redirect to login.
	if w.Code != http.StatusSeeOther {
		t.Fatalf("hard-cap GET /m/2fa/verify: status = %d, want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/m/login") {
		t.Fatalf("hard-cap redirect Location = %q, want /m/login*", loc)
	}
	sc := w.Header().Get("Set-Cookie")
	if !strings.Contains(sc, "__Host-sess-master") || !strings.Contains(sc, "Max-Age=0") {
		t.Fatalf("hard-cap must clear the master cookie; Set-Cookie = %q", sc)
	}

	// Defense-in-depth control #2: exactly one hard-cap-hit audit row,
	// audience="master", nil tenant, attributed to the session operator.
	var hits []audit.SecurityAuditEvent
	for _, e := range rec.security {
		if e.Event == audit.SecurityEventMasterSessionHardCapHit {
			hits = append(hits, e)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("hard-cap audit rows = %d, want exactly 1", len(hits))
	}
	got := hits[0]
	if got.Target["audience"] != "master" {
		t.Errorf("hard-cap audience = %v, want master", got.Target["audience"])
	}
	if got.TenantID != nil {
		t.Errorf("hard-cap TenantID = %v, want nil (master rows are tenant-less)", got.TenantID)
	}
	if got.ActorUserID != userID {
		t.Errorf("hard-cap ActorUserID = %v, want %v", got.ActorUserID, userID)
	}
	if got.Target["route"] != "/m/2fa/verify" {
		t.Errorf("hard-cap route = %v, want /m/2fa/verify", got.Target["route"])
	}
}

// TestBuildMasterDeps_LogoutIsPostOnly locks in the SIN-65232 CSRF fix: a
// GET to /m/logout (the cross-site <img>/link forced-logout vector) is
// rejected at the router with 405 and never reaches the handler, so no
// session is torn down. The POST path is exercised by
// TestBuildMasterDeps_LogoutAppendsMasterAudienceRow.
func TestBuildMasterDeps_LogoutIsPostOnly(t *testing.T) {
	t.Parallel()

	sessionID := uuid.New()
	userID := uuid.New()
	sessions := &stubMasterSessions{
		session: mastermfa.Session{ID: sessionID, UserID: userID},
	}
	stack := newTestStack(sessions, stubEnrollment{seed: []byte("seed")}, stubDirectory{email: "op@example.com"})
	deps, _ := buildMasterDeps(stack, &recordingSplitLogger{}, slogTestLogger(), "master.crm.local")
	h := mountMaster(t, deps)

	req := httptest.NewRequest(http.MethodGet, "/m/logout", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-sess-master", Value: sessionID.String()})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /m/logout: status = %d, want 405 (POST-only)", w.Code)
	}
	// The forced GET must NOT have deleted the session.
	if len(sessions.deleted) != 0 {
		t.Fatalf("GET /m/logout deleted %d sessions, want 0 (handler must be unreachable)", len(sessions.deleted))
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
	deps, _ := buildMasterDeps(stack, &recordingSplitLogger{}, slogTestLogger(), "master.crm.local")
	h := mountMaster(t, deps)

	// POST because the enrol handler is POST-only (it mints fresh codes).
	req := mPost("/m/2fa/enroll", nil)
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
