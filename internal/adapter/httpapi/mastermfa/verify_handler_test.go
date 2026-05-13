package mastermfa_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// fakeVerifier scripts mfa.Service.Verify.
type fakeVerifier struct {
	calls    int
	lastUID  uuid.UUID
	lastCode string
	err      error
}

func (f *fakeVerifier) Verify(_ context.Context, uid uuid.UUID, code string) error {
	f.calls++
	f.lastUID = uid
	f.lastCode = code
	return f.err
}

// fakeConsumer scripts mfa.Service.ConsumeRecovery.
type fakeConsumer struct {
	calls      int
	lastUID    uuid.UUID
	lastCode   string
	lastReqCtx mfa.RequestContext
	err        error
}

func (f *fakeConsumer) ConsumeRecovery(_ context.Context, uid uuid.UUID, code string, reqCtx mfa.RequestContext) error {
	f.calls++
	f.lastUID = uid
	f.lastCode = code
	f.lastReqCtx = reqCtx
	return f.err
}

func newVerifyHandler(t *testing.T, v *fakeVerifier, c *fakeConsumer, s *fakeSessions) *mastermfa.VerifyHandler {
	t.Helper()
	return mastermfa.NewVerifyHandler(mastermfa.VerifyHandlerConfig{
		Verifier:   v,
		Consumer:   c,
		Sessions:   s,
		FallbackOK: "/m/dashboard",
	})
}

func postVerify(target string, body url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	uid := uuid.New()
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uid, Email: "ops@example.com"}))
	return r
}

func TestVerifyHandler_PanicsOnNilDeps(t *testing.T) {
	cases := map[string]mastermfa.VerifyHandlerConfig{
		"nil verifier": {Consumer: &fakeConsumer{}, Sessions: &fakeSessions{}},
		"nil consumer": {Verifier: &fakeVerifier{}, Sessions: &fakeSessions{}},
		"nil sessions": {Verifier: &fakeVerifier{}, Consumer: &fakeConsumer{}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic")
				}
			}()
			mastermfa.NewVerifyHandler(cfg)
		})
	}
}

func TestVerifyHandler_GetRendersForm(t *testing.T) {
	h := newVerifyHandler(t, &fakeVerifier{}, &fakeConsumer{}, &fakeSessions{})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/2fa/verify", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), `<form method="POST"`) {
		t.Error("body missing verify form")
	}
}

func TestVerifyHandler_RejectsOtherMethods(t *testing.T) {
	h := newVerifyHandler(t, &fakeVerifier{}, &fakeConsumer{}, &fakeSessions{})
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(method, "/m/2fa/verify", nil)
			h.ServeHTTP(w, r)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("status: got %d want 405", w.Code)
			}
		})
	}
}

func TestVerifyHandler_PostMissingMaster_Returns401(t *testing.T) {
	h := newVerifyHandler(t, &fakeVerifier{}, &fakeConsumer{}, &fakeSessions{})
	w := httptest.NewRecorder()
	body := url.Values{"code": {"287082"}}
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", w.Code)
	}
}

func TestVerifyHandler_HappyPath_TOTP_RedirectsAndMarksVerified(t *testing.T) {
	v := &fakeVerifier{}
	c := &fakeConsumer{}
	s := &fakeSessions{}
	h := newVerifyHandler(t, v, c, s)
	w := httptest.NewRecorder()
	r := postVerify("/m/2fa/verify?return=/m/tenant", url.Values{"code": {"287082"}})
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/m/tenant" {
		t.Errorf("Location: got %q want /m/tenant", loc)
	}
	if v.calls != 1 {
		t.Errorf("Verify calls: got %d want 1", v.calls)
	}
	if c.calls != 0 {
		t.Errorf("ConsumeRecovery calls: got %d want 0 (TOTP shape)", c.calls)
	}
	if s.markCalls != 1 {
		t.Errorf("MarkVerified calls: got %d want 1", s.markCalls)
	}
}

func TestVerifyHandler_HappyPath_RecoveryCode(t *testing.T) {
	v := &fakeVerifier{}
	c := &fakeConsumer{}
	s := &fakeSessions{}
	h := newVerifyHandler(t, v, c, s)
	w := httptest.NewRecorder()
	r := postVerify("/m/2fa/verify", url.Values{"code": {"ABCDE-23456"}})
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", w.Code)
	}
	if c.calls != 1 {
		t.Errorf("ConsumeRecovery calls: got %d want 1 (recovery shape)", c.calls)
	}
	if v.calls != 0 {
		t.Errorf("Verify calls: got %d want 0", v.calls)
	}
	if c.lastCode != "ABCDE-23456" {
		t.Errorf("ConsumeRecovery code: got %q want %q (consumer normalises)", c.lastCode, "ABCDE-23456")
	}
}

func TestVerifyHandler_FallbackOKWhenNoReturn(t *testing.T) {
	v := &fakeVerifier{}
	h := newVerifyHandler(t, v, &fakeConsumer{}, &fakeSessions{})
	w := httptest.NewRecorder()
	r := postVerify("/m/2fa/verify", url.Values{"code": {"287082"}})
	h.ServeHTTP(w, r)
	if loc := w.Header().Get("Location"); loc != "/m/dashboard" {
		t.Errorf("Location: got %q want /m/dashboard (FallbackOK)", loc)
	}
}

func TestVerifyHandler_RejectsOpenRedirectReturn(t *testing.T) {
	v := &fakeVerifier{}
	h := newVerifyHandler(t, v, &fakeConsumer{}, &fakeSessions{})
	w := httptest.NewRecorder()
	r := postVerify("/m/2fa/verify?return=https://evil.com/x", url.Values{"code": {"287082"}})
	h.ServeHTTP(w, r)
	if loc := w.Header().Get("Location"); loc != "/m/dashboard" {
		t.Errorf("Location: got %q want /m/dashboard (rejected absolute URL)", loc)
	}
}

func TestVerifyHandler_TOTPInvalidCodeReRendersForm(t *testing.T) {
	v := &fakeVerifier{err: mfa.ErrInvalidCode}
	h := newVerifyHandler(t, v, &fakeConsumer{}, &fakeSessions{})
	w := httptest.NewRecorder()
	r := postVerify("/m/2fa/verify", url.Values{"code": {"000000"}})
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), "código inválido") {
		t.Error("body missing generic error message")
	}
}

func TestVerifyHandler_RecoveryInvalidCodeReRendersForm(t *testing.T) {
	c := &fakeConsumer{err: mfa.ErrInvalidCode}
	h := newVerifyHandler(t, &fakeVerifier{}, c, &fakeSessions{})
	w := httptest.NewRecorder()
	r := postVerify("/m/2fa/verify", url.Values{"code": {"ZZZZZ-77777"}})
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), "código inválido") {
		t.Error("body missing generic error message")
	}
	if c.calls != 1 {
		t.Errorf("ConsumeRecovery calls: got %d want 1", c.calls)
	}
}

func TestVerifyHandler_EmptyCodeReRendersWithError(t *testing.T) {
	v := &fakeVerifier{}
	h := newVerifyHandler(t, v, &fakeConsumer{}, &fakeSessions{})
	w := httptest.NewRecorder()
	r := postVerify("/m/2fa/verify", url.Values{"code": {""}})
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", w.Code)
	}
	if v.calls != 0 {
		t.Errorf("Verify calls: got %d want 0 (empty code short-circuits)", v.calls)
	}
}

func TestVerifyHandler_VerifyInternalErrorReturns500(t *testing.T) {
	v := &fakeVerifier{err: errors.New("decrypt boom")}
	h := newVerifyHandler(t, v, &fakeConsumer{}, &fakeSessions{})
	w := httptest.NewRecorder()
	r := postVerify("/m/2fa/verify", url.Values{"code": {"287082"}})
	h.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "decrypt boom") {
		t.Errorf("body leaked internal error: %q", w.Body.String())
	}
}

func TestVerifyHandler_ConsumerInternalErrorReturns500(t *testing.T) {
	c := &fakeConsumer{err: errors.New("argon decode boom")}
	h := newVerifyHandler(t, &fakeVerifier{}, c, &fakeSessions{})
	w := httptest.NewRecorder()
	r := postVerify("/m/2fa/verify", url.Values{"code": {"ABCDE-23456"}})
	h.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", w.Code)
	}
}

func TestVerifyHandler_MarkVerifiedFailureReturns500(t *testing.T) {
	v := &fakeVerifier{}
	s := &fakeSessions{markErr: errors.New("session blip")}
	h := newVerifyHandler(t, v, &fakeConsumer{}, s)
	w := httptest.NewRecorder()
	r := postVerify("/m/2fa/verify", url.Values{"code": {"287082"}})
	h.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", w.Code)
	}
}

func TestVerifyHandler_NonSixDigitGoesToConsumer(t *testing.T) {
	// Even partially-numeric codes shorter or longer than 6 digits
	// get routed to ConsumeRecovery — the dispatch is exact-match on
	// shape. The consumer normalises and refuses non-base32.
	cases := []string{"12345" /* 5 */, "1234567" /* 7 */, "abcdef" /* 6 alpha */, "12-34" /* dashed */}
	for _, code := range cases {
		t.Run(code, func(t *testing.T) {
			c := &fakeConsumer{err: mfa.ErrInvalidCode}
			h := newVerifyHandler(t, &fakeVerifier{}, c, &fakeSessions{})
			w := httptest.NewRecorder()
			r := postVerify("/m/2fa/verify", url.Values{"code": {code}})
			h.ServeHTTP(w, r)
			if c.calls != 1 {
				t.Errorf("input %q: consumer calls %d want 1", code, c.calls)
			}
		})
	}
}

func TestVerifyHandler_FormCacheHeaders(t *testing.T) {
	h := newVerifyHandler(t, &fakeVerifier{}, &fakeConsumer{}, &fakeSessions{})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/m/2fa/verify", nil)
	h.ServeHTTP(w, r)
	cc := w.Header().Get("Cache-Control")
	if !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control: got %q, expected no-store", cc)
	}
}

func TestVerifyHandler_BadFormBody_Returns400(t *testing.T) {
	h := newVerifyHandler(t, &fakeVerifier{}, &fakeConsumer{}, &fakeSessions{})
	w := httptest.NewRecorder()
	uid := uuid.New()
	// Construct a request whose ParseForm would fail (illegal Content-Type
	// makes ParseForm refuse to parse the body — but it doesn't error for
	// unknown content types, it just leaves PostForm empty. To force an
	// error, give it a bad URL-encoded body.)
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", strings.NewReader("%ZZ"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uid, Email: "ops"}))
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}
