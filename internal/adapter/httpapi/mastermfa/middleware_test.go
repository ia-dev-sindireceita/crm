package mastermfa_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// fakeEnrollment returns a scripted result for LoadSeed.
type fakeEnrollment struct {
	calls   int
	loadErr error
}

func (f *fakeEnrollment) LoadSeed(_ context.Context, _ uuid.UUID) ([]byte, error) {
	f.calls++
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return []byte("opaque-ciphertext"), nil
}

// fakeSessions tracks IsVerified / MarkVerified calls.
type fakeSessions struct {
	verified        bool
	verifiedErr     error
	markCalls       int
	markErr         error
	isVerifiedCalls int
}

func (f *fakeSessions) IsVerified(_ *http.Request) (bool, error) {
	f.isVerifiedCalls++
	if f.verifiedErr != nil {
		return false, f.verifiedErr
	}
	return f.verified, nil
}
func (f *fakeSessions) MarkVerified(_ http.ResponseWriter, _ *http.Request) error {
	f.markCalls++
	return f.markErr
}

// fakeAuditor records LogMFARequired calls so we can assert
// reason="not_enrolled" vs "not_verified".
type fakeAuditor struct {
	calls      int
	lastUser   uuid.UUID
	lastRoute  string
	lastReason string
}

func (f *fakeAuditor) LogMFARequired(_ context.Context, uid uuid.UUID, route, reason string) error {
	f.calls++
	f.lastUser = uid
	f.lastRoute = route
	f.lastReason = reason
	return nil
}

func newMiddlewareUnderTest(enroll *fakeEnrollment, sessions *fakeSessions, audit *fakeAuditor) func(http.Handler) http.Handler {
	return mastermfa.RequireMasterMFA(mastermfa.RequireMasterMFAConfig{
		Enrollment: enroll,
		Sessions:   sessions,
		Audit:      audit,
	})
}

// downstream is an http.Handler that records whether it was reached.
// The middleware MUST NOT call this when the gate denies — every test
// asserts on calls == 0 for denied cases.
type downstream struct{ calls int }

func (d *downstream) ServeHTTP(_ http.ResponseWriter, _ *http.Request) { d.calls++ }

func makeReqWithMaster(target string) (*http.Request, uuid.UUID) {
	uid := uuid.New()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uid, Email: "ops@example.com"}))
	return r, uid
}

// ---------------------------------------------------------------------------
// Constructor preconditions
// ---------------------------------------------------------------------------

func TestRequireMasterMFA_PanicsOnMissingDeps(t *testing.T) {
	cases := map[string]mastermfa.RequireMasterMFAConfig{
		"nil enrollment": {Sessions: &fakeSessions{}, Audit: &fakeAuditor{}},
		"nil sessions":   {Enrollment: &fakeEnrollment{}, Audit: &fakeAuditor{}},
		"nil audit":      {Enrollment: &fakeEnrollment{}, Sessions: &fakeSessions{}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic, got none")
				}
			}()
			mastermfa.RequireMasterMFA(cfg)
		})
	}
}

// ---------------------------------------------------------------------------
// Auth gate (Step 1)
// ---------------------------------------------------------------------------

func TestRequireMasterMFA_NoMaster_Returns401(t *testing.T) {
	enroll := &fakeEnrollment{}
	mw := newMiddlewareUnderTest(enroll, &fakeSessions{verified: true}, &fakeAuditor{})
	d := &downstream{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/tenant", nil) // no master in context
	mw(d).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", w.Code)
	}
	if d.calls != 0 {
		t.Fatal("downstream reached without master in context")
	}
	if enroll.calls != 0 {
		t.Fatal("enrollment lookup ran without auth")
	}
}

// ---------------------------------------------------------------------------
// Enrolment gate (Step 2) — AC #1
// ---------------------------------------------------------------------------

func TestRequireMasterMFA_NotEnrolled_RedirectsToEnroll(t *testing.T) {
	enroll := &fakeEnrollment{loadErr: mfa.ErrNotEnrolled}
	sessions := &fakeSessions{verified: true}
	audit := &fakeAuditor{}
	mw := newMiddlewareUnderTest(enroll, sessions, audit)
	d := &downstream{}
	r, uid := makeReqWithMaster("/m/tenant")
	w := httptest.NewRecorder()
	mw(d).ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303 (See Other)", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/m/2fa/enroll?return=") {
		t.Errorf("Location: got %q, expected /m/2fa/enroll?return=...", loc)
	}
	parsed, _ := url.Parse(loc)
	if got := parsed.Query().Get("return"); got != "/m/tenant" {
		t.Errorf("return param: got %q want /m/tenant", got)
	}
	if d.calls != 0 {
		t.Fatal("downstream reached for not-enrolled master")
	}
	// Audit fired with reason="not_enrolled".
	if audit.calls != 1 {
		t.Fatalf("audit calls: got %d want 1", audit.calls)
	}
	if audit.lastReason != mastermfa.ReasonNotEnrolled {
		t.Errorf("audit reason: got %q want %q", audit.lastReason, mastermfa.ReasonNotEnrolled)
	}
	if audit.lastRoute != "/m/tenant" {
		t.Errorf("audit route: got %q want /m/tenant", audit.lastRoute)
	}
	if audit.lastUser != uid {
		t.Errorf("audit user_id mismatch")
	}
	// IsVerified MUST NOT run if the enrolment gate already denied.
	if sessions.isVerifiedCalls != 0 {
		t.Errorf("session check ran after enrolment denial: %d", sessions.isVerifiedCalls)
	}
}

func TestRequireMasterMFA_EnrollmentReadFailure_Returns500(t *testing.T) {
	enroll := &fakeEnrollment{loadErr: errors.New("db down")}
	mw := newMiddlewareUnderTest(enroll, &fakeSessions{}, &fakeAuditor{})
	d := &downstream{}
	r, _ := makeReqWithMaster("/m/tenant")
	w := httptest.NewRecorder()
	mw(d).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", w.Code)
	}
	if d.calls != 0 {
		t.Fatal("downstream reached on storage failure")
	}
}

// ---------------------------------------------------------------------------
// Session-verified gate (Step 3) — AC #5 prep
// ---------------------------------------------------------------------------

func TestRequireMasterMFA_NotSessionVerified_RedirectsToVerify(t *testing.T) {
	enroll := &fakeEnrollment{}
	sessions := &fakeSessions{verified: false}
	audit := &fakeAuditor{}
	mw := newMiddlewareUnderTest(enroll, sessions, audit)
	d := &downstream{}
	r, _ := makeReqWithMaster("/m/impersonate/foo")
	w := httptest.NewRecorder()
	mw(d).ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/m/2fa/verify?return=") {
		t.Errorf("Location: got %q, expected /m/2fa/verify?return=...", loc)
	}
	if d.calls != 0 {
		t.Fatal("downstream reached for unverified session")
	}
	if audit.lastReason != mastermfa.ReasonNotVerified {
		t.Errorf("audit reason: got %q want %q", audit.lastReason, mastermfa.ReasonNotVerified)
	}
}

func TestRequireMasterMFA_SessionReadFailure_Returns500(t *testing.T) {
	enroll := &fakeEnrollment{}
	sessions := &fakeSessions{verifiedErr: errors.New("session store down")}
	mw := newMiddlewareUnderTest(enroll, sessions, &fakeAuditor{})
	d := &downstream{}
	r, _ := makeReqWithMaster("/m/tenant")
	w := httptest.NewRecorder()
	mw(d).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", w.Code)
	}
	if d.calls != 0 {
		t.Fatal("downstream reached on session-store failure")
	}
}

// ---------------------------------------------------------------------------
// Allow path
// ---------------------------------------------------------------------------

func TestRequireMasterMFA_AllowsWhenEnrolledAndVerified(t *testing.T) {
	enroll := &fakeEnrollment{}
	sessions := &fakeSessions{verified: true}
	audit := &fakeAuditor{}
	mw := newMiddlewareUnderTest(enroll, sessions, audit)
	d := &downstream{}
	r, _ := makeReqWithMaster("/m/tenant")
	w := httptest.NewRecorder()
	mw(d).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	if d.calls != 1 {
		t.Fatalf("downstream calls: got %d want 1", d.calls)
	}
	if audit.calls != 0 {
		t.Errorf("audit fired on allow path: %d", audit.calls)
	}
}

// ---------------------------------------------------------------------------
// Custom enroll/verify path overrides
// ---------------------------------------------------------------------------

func TestRequireMasterMFA_HonoursCustomPaths(t *testing.T) {
	enroll := &fakeEnrollment{loadErr: mfa.ErrNotEnrolled}
	sessions := &fakeSessions{}
	audit := &fakeAuditor{}
	mw := mastermfa.RequireMasterMFA(mastermfa.RequireMasterMFAConfig{
		Enrollment: enroll,
		Sessions:   sessions,
		Audit:      audit,
		EnrollPath: "/custom/enroll",
		VerifyPath: "/custom/verify",
	})
	d := &downstream{}
	r, _ := makeReqWithMaster("/m/tenant")
	w := httptest.NewRecorder()
	mw(d).ServeHTTP(w, r)
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/custom/enroll") {
		t.Errorf("custom enroll path not honoured: %q", loc)
	}
}

// ---------------------------------------------------------------------------
// Open-redirect defence
// ---------------------------------------------------------------------------

func TestRequireMasterMFA_RejectsAbsoluteReturnURLs(t *testing.T) {
	// A hostile request with a Host header set to evil.com would have
	// r.URL.RequestURI() = "/m/tenant", which is safe. The defence
	// matters most on the *return* path: ResolveReturn refuses
	// absolute URLs. We probe that here with the helper directly.
	cases := map[string]string{
		"absolute-https":   "https://evil.com/x",
		"absolute-http":    "http://evil.com/x",
		"scheme-relative":  "//evil.com/x",
		"no-leading-slash": "evil",
		"empty":            "",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got := mastermfa.ResolveReturn(in, "/fallback")
			if got != "/fallback" {
				t.Errorf("ResolveReturn(%q): got %q, want fallback", in, got)
			}
		})
	}
}

func TestRequireMasterMFA_AcceptsSafeRelativePaths(t *testing.T) {
	cases := map[string]string{
		"plain":         "/m/tenant",
		"with-query":    "/m/tenant?foo=bar",
		"with-fragment": "/m/tenant#x",
		"with-encoded":  url.QueryEscape("/m/tenant?foo=bar"),
		"colon-in-path": "/m/tenant:colon", // colon AFTER first slash is fine
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got := mastermfa.ResolveReturn(in, "/fallback")
			// We want the original (decoded) path back.
			decoded, _ := url.QueryUnescape(in)
			if got != decoded {
				t.Errorf("ResolveReturn(%q): got %q want %q", in, got, decoded)
			}
		})
	}
}
