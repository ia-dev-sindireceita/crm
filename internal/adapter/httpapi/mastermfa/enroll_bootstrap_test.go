package mastermfa_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// SIN-65264 Bug 2 — the enrol path must NOT be redirected to itself when
// the master is not enrolled (that was the infinite 303 loop). The
// middleware self-excludes the enrol path and falls through to the
// (auth-gated) enrol handler.
func TestRequireMasterMFA_NotEnrolled_AllowsEnrollPath_NoLoop(t *testing.T) {
	enroll := &fakeEnrollment{loadErr: mfa.ErrNotEnrolled}
	sessions := &fakeSessions{}
	audit := &fakeAuditor{}
	mw := newMiddlewareUnderTest(enroll, sessions, audit)
	d := &downstream{}
	// Default enrollPath is /m/2fa/enroll.
	r, _ := makeReqWithMaster("/m/2fa/enroll")
	w := httptest.NewRecorder()
	mw(d).ServeHTTP(w, r)

	if d.calls != 1 {
		t.Fatalf("downstream calls: got %d want 1 (enrol handler must be reached)", d.calls)
	}
	if w.Code == http.StatusSeeOther {
		t.Fatalf("got 303 redirect on enrol path — the loop is back")
	}
	if audit.calls != 0 {
		t.Errorf("audit calls: got %d want 0 (self-exclusion must not log a denial)", audit.calls)
	}
	// The enrolment gate decided the outcome; the session-verified check
	// MUST NOT run (a not-enrolled master has no verified session yet).
	if sessions.isVerifiedCalls != 0 {
		t.Errorf("IsVerified ran on the self-excluded enrol path: %d", sessions.isVerifiedCalls)
	}
}

// The self-exclusion honours a custom EnrollPath (callers may mount the
// handler at a non-default route).
func TestRequireMasterMFA_NotEnrolled_SelfExcludesCustomEnrollPath(t *testing.T) {
	enroll := &fakeEnrollment{loadErr: mfa.ErrNotEnrolled}
	mw := mastermfa.RequireMasterMFA(mastermfa.RequireMasterMFAConfig{
		Enrollment: enroll,
		Sessions:   &fakeSessions{},
		Audit:      &fakeAuditor{},
		EnrollPath: "/custom/enroll",
	})
	d := &downstream{}
	r, _ := makeReqWithMaster("/custom/enroll")
	w := httptest.NewRecorder()
	mw(d).ServeHTTP(w, r)
	if d.calls != 1 {
		t.Fatalf("downstream calls: got %d want 1 on custom enrol path", d.calls)
	}
	if w.Code == http.StatusSeeOther {
		t.Fatalf("got 303 redirect on custom enrol path — self-exclusion missed it")
	}
}

// A not-enrolled master hitting any OTHER master route is still
// redirected to enroll (the self-exclusion is path-scoped, not a blanket
// bypass) — guards against the exclusion accidentally opening the gate.
func TestRequireMasterMFA_NotEnrolled_NonEnrollPath_StillRedirects(t *testing.T) {
	enroll := &fakeEnrollment{loadErr: mfa.ErrNotEnrolled}
	audit := &fakeAuditor{}
	mw := newMiddlewareUnderTest(enroll, &fakeSessions{}, audit)
	d := &downstream{}
	r, _ := makeReqWithMaster("/m/tenants")
	w := httptest.NewRecorder()
	mw(d).ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303 (not-enrolled on a gated route)", w.Code)
	}
	if d.calls != 0 {
		t.Fatalf("downstream reached for not-enrolled master on gated route")
	}
	if !strings.HasPrefix(w.Header().Get("Location"), "/m/2fa/enroll?return=") {
		t.Errorf("Location: got %q want /m/2fa/enroll?return=...", w.Header().Get("Location"))
	}
	if audit.calls != 1 || audit.lastReason != mastermfa.ReasonNotEnrolled {
		t.Errorf("audit: calls=%d reason=%q want 1 / not_enrolled", audit.calls, audit.lastReason)
	}
}

// --- EnrollStartHandler (GET /m/2fa/enroll server-rendered bootstrap) ---

func newEnrollStartReq(method, target string, withMaster bool) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	if withMaster {
		r = r.WithContext(mastermfa.WithMaster(r.Context(),
			mastermfa.Master{ID: uuid.New(), Email: "ops@example.com"}))
	}
	return r
}

func TestEnrollStartHandler_GET_Renders200WithPostForm(t *testing.T) {
	h := mastermfa.NewEnrollStartHandler(nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newEnrollStartReq(http.MethodGet, "/m/2fa/enroll", true))

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `method="post"`) || !strings.Contains(body, `action="/m/2fa/enroll"`) {
		t.Errorf("body missing POST form to /m/2fa/enroll:\n%s", body)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control: got %q want no-store…", cc)
	}
}

func TestEnrollStartHandler_RejectsNonGet(t *testing.T) {
	h := mastermfa.NewEnrollStartHandler(nil)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newEnrollStartReq(m, "/m/2fa/enroll", true))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status got %d want 405", m, w.Code)
		}
		if got := w.Header().Get("Allow"); got != http.MethodGet {
			t.Errorf("%s: Allow got %q want GET", m, got)
		}
	}
}

func TestEnrollStartHandler_Returns401WhenNoMaster(t *testing.T) {
	h := mastermfa.NewEnrollStartHandler(nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newEnrollStartReq(http.MethodGet, "/m/2fa/enroll", false))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", w.Code)
	}
}

func TestEnrollStartHandler_ThreadsSafeReturn(t *testing.T) {
	h := mastermfa.NewEnrollStartHandler(nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newEnrollStartReq(http.MethodGet, "/m/2fa/enroll?return=%2Fmaster%2Ftenants", true))
	body := w.Body.String()
	if !strings.Contains(body, `name="return" value="/master/tenants"`) {
		t.Errorf("safe return not threaded into hidden field:\n%s", body)
	}
}

func TestEnrollStartHandler_DropsUnsafeReturn(t *testing.T) {
	h := mastermfa.NewEnrollStartHandler(nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newEnrollStartReq(http.MethodGet, "/m/2fa/enroll?return=https%3A%2F%2Fevil.example", true))
	if strings.Contains(w.Body.String(), "evil.example") {
		t.Errorf("absolute/off-site return leaked into the form")
	}
}
