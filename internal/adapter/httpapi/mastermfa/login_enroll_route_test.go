package mastermfa_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// loginWithEnrollment builds a master login handler whose post-login
// redirect is enrollment-aware (SIN-65264 Bug 2c).
func loginWithEnrollment(t *testing.T, enroll mastermfa.EnrollmentReader) *mastermfa.LoginHandler {
	t.Helper()
	login := &fakeMasterLogin{result: iam.Session{UserID: uuid.New()}}
	return mastermfa.NewLoginHandler(mastermfa.LoginHandlerConfig{
		Login:      login.Login,
		Sessions:   newFakeSessionStore(),
		HardTTL:    time.Hour,
		Enrollment: enroll,
		Logger:     silentLogger(),
	})
}

func postLogin(t *testing.T, h http.Handler, next string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	form.Set("email", "ops@example.com")
	form.Set("password", "correct horse")
	target := "/m/login"
	if next != "" {
		target += "?next=" + url.QueryEscape(next)
	}
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "203.0.113.5:12345"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestLogin_NotEnrolled_RoutesToEnroll(t *testing.T) {
	h := loginWithEnrollment(t, &fakeEnrollment{loadErr: mfa.ErrNotEnrolled})
	w := postLogin(t, h, "")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/m/2fa/enroll" {
		t.Errorf("Location: got %q want /m/2fa/enroll (not-enrolled bootstrap)", loc)
	}
}

func TestLogin_Enrolled_RoutesToVerify(t *testing.T) {
	// fakeEnrollment with no loadErr returns a seed → enrolled.
	h := loginWithEnrollment(t, &fakeEnrollment{})
	w := postLogin(t, h, "")
	if loc := w.Header().Get("Location"); loc != "/m/2fa/verify" {
		t.Errorf("Location: got %q want /m/2fa/verify (enrolled)", loc)
	}
}

// nil Enrollment preserves the legacy always-verify behaviour (router
// tests / minimal wireups depend on it).
func TestLogin_NilEnrollment_LegacyVerify(t *testing.T) {
	login := &fakeMasterLogin{result: iam.Session{UserID: uuid.New()}}
	h := mastermfa.NewLoginHandler(mastermfa.LoginHandlerConfig{
		Login:    login.Login,
		Sessions: newFakeSessionStore(),
		HardTTL:  time.Hour,
		Logger:   silentLogger(),
	})
	w := postLogin(t, h, "")
	if loc := w.Header().Get("Location"); loc != "/m/2fa/verify" {
		t.Errorf("Location: got %q want /m/2fa/verify (nil enrollment legacy)", loc)
	}
}

// The bootstrap ?next= is threaded onto the enroll destination so the
// operator lands on the originally-requested URL after enroll → verify.
func TestLogin_NotEnrolled_ThreadsReturnToEnroll(t *testing.T) {
	h := loginWithEnrollment(t, &fakeEnrollment{loadErr: mfa.ErrNotEnrolled})
	w := postLogin(t, h, "/master/tenants")
	loc := w.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location %q: %v", loc, err)
	}
	if u.Path != "/m/2fa/enroll" {
		t.Errorf("path: got %q want /m/2fa/enroll", u.Path)
	}
	if got := u.Query().Get("return"); got != "/master/tenants" {
		t.Errorf("return: got %q want /master/tenants", got)
	}
}
