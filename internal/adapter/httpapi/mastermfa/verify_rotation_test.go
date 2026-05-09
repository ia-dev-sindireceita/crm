package mastermfa_test

// SIN-62377 (FAIL-4) verify-handler rotation tests. Cover:
//   - Rotator wired: success path swaps cookie value and DOES NOT call
//     the legacy MarkVerified (single source of truth on rotation).
//   - Rotator wired: rotator error → 500, downstream redirect not issued.
//   - Rotator NOT wired: legacy MarkVerified path still works (back-compat).

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
)

// fakeRotator scripts MasterSessionRotator. It records call count,
// optionally returns an error, and on success writes a Set-Cookie
// with the master cookie name + a fixed sentinel value so the test
// can prove rotation occurred without spinning up the real
// adapter.
type fakeRotator struct {
	calls       int
	err         error
	cookieValue string // value to write on success (must be non-empty)
}

func (f *fakeRotator) RotateAndMarkVerified(w http.ResponseWriter, _ *http.Request) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	v := f.cookieValue
	if v == "" {
		v = "rotated-cookie-value"
	}
	sessioncookie.SetMaster(w, v, 14400)
	return nil
}

// On 2FA success with Rotator wired, the cookie is rotated and the
// legacy MarkVerified is NOT called (single source of truth).
func TestVerifyHandler_Rotator_Success_RotatesCookieAndSkipsLegacyMark(t *testing.T) {
	v := &fakeVerifier{}
	c := &fakeConsumer{}
	s := &fakeSessions{} // legacy MarkVerified port; must NOT be touched
	rot := &fakeRotator{cookieValue: "post-mfa-id-abc"}
	h := mastermfa.NewVerifyHandler(mastermfa.VerifyHandlerConfig{
		Verifier:   v,
		Consumer:   c,
		Sessions:   s,
		Rotator:    rot,
		FallbackOK: "/m/dashboard",
	})

	w := httptest.NewRecorder()
	r := postVerify("/m/2fa/verify", url.Values{"code": {"287082"}})
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if rot.calls != 1 {
		t.Fatalf("Rotator calls = %d, want 1", rot.calls)
	}
	if s.markCalls != 0 {
		t.Fatalf("legacy MarkVerified called %d times; rotation path must NOT touch it", s.markCalls)
	}
	// The cookie must now carry the rotated value.
	var got string
	for _, sc := range w.Result().Cookies() {
		if sc.Name == sessioncookie.NameMaster {
			got = sc.Value
		}
	}
	if got != "post-mfa-id-abc" {
		t.Fatalf("master cookie value = %q, want rotated value %q", got, "post-mfa-id-abc")
	}
}

// Recovery-code path uses the same rotation hook.
func TestVerifyHandler_Rotator_RecoveryCode_RotatesCookie(t *testing.T) {
	v := &fakeVerifier{}
	c := &fakeConsumer{}
	s := &fakeSessions{}
	rot := &fakeRotator{cookieValue: "post-recovery-xyz"}
	h := mastermfa.NewVerifyHandler(mastermfa.VerifyHandlerConfig{
		Verifier:   v,
		Consumer:   c,
		Sessions:   s,
		Rotator:    rot,
		FallbackOK: "/m/dashboard",
	})
	w := httptest.NewRecorder()
	r := postVerify("/m/2fa/verify", url.Values{"code": {"ABCDE-23456"}})
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if rot.calls != 1 {
		t.Fatalf("Rotator calls = %d, want 1 (recovery success)", rot.calls)
	}
	if c.calls != 1 {
		t.Fatalf("ConsumeRecovery calls = %d, want 1", c.calls)
	}
	if s.markCalls != 0 {
		t.Fatalf("legacy MarkVerified called %d times on rotation path", s.markCalls)
	}
}

// Rotator failure → 500 (deny-by-default; must not redirect / set
// any cookie other than possibly cleared ones).
func TestVerifyHandler_Rotator_Error_Returns500NoRedirect(t *testing.T) {
	v := &fakeVerifier{}
	c := &fakeConsumer{}
	s := &fakeSessions{}
	rot := &fakeRotator{err: errors.New("storage transient")}
	h := mastermfa.NewVerifyHandler(mastermfa.VerifyHandlerConfig{
		Verifier:   v,
		Consumer:   c,
		Sessions:   s,
		Rotator:    rot,
		FallbackOK: "/m/dashboard",
	})
	w := httptest.NewRecorder()
	r := postVerify("/m/2fa/verify", url.Values{"code": {"287082"}})
	h.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if rot.calls != 1 {
		t.Fatalf("Rotator calls = %d, want 1", rot.calls)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Fatalf("Location header set on rotator failure: %q", loc)
	}
}

// Back-compat: when no Rotator is supplied, the legacy MarkVerified
// path is exercised — same behaviour as pre-SIN-62377.
func TestVerifyHandler_NoRotator_FallsBackToLegacyMarkVerified(t *testing.T) {
	v := &fakeVerifier{}
	c := &fakeConsumer{}
	s := &fakeSessions{}
	h := mastermfa.NewVerifyHandler(mastermfa.VerifyHandlerConfig{
		Verifier:   v,
		Consumer:   c,
		Sessions:   s,
		FallbackOK: "/m/dashboard",
		// Rotator intentionally nil
	})
	w := httptest.NewRecorder()
	r := postVerify("/m/2fa/verify", url.Values{"code": {"287082"}})
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if s.markCalls != 1 {
		t.Fatalf("legacy MarkVerified calls = %d, want 1 (no rotator wired)", s.markCalls)
	}
}
