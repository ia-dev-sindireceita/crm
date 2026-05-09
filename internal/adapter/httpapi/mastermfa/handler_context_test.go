package mastermfa_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
)

// TestVerifyHandler_ConsumerReceivesRequestContext is the SIN-62382
// handler-side guarantee: the IP / user-agent / route reaching
// ConsumeRecovery come from the inbound request, not from a
// hard-coded constant. CAVEAT-5 in the SIN-62343 security review.
func TestVerifyHandler_ConsumerReceivesRequestContext(t *testing.T) {
	c := &fakeConsumer{}
	h := newVerifyHandler(t, &fakeVerifier{}, c, &fakeSessions{})

	body := url.Values{"code": {"ABCDE-23456"}}
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64)")
	r.RemoteAddr = "203.0.113.5:51234"
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{
		ID:    uuid.New(),
		Email: "ops@example.com",
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if c.calls != 1 {
		t.Fatalf("ConsumeRecovery calls: got %d want 1", c.calls)
	}
	if got := c.lastReqCtx.IP; got != "203.0.113.5" {
		t.Errorf("IP: got %q want %q (port stripped)", got, "203.0.113.5")
	}
	if got := c.lastReqCtx.UserAgent; got != "Mozilla/5.0 (X11; Linux x86_64)" {
		t.Errorf("UserAgent: got %q", got)
	}
	if got := c.lastReqCtx.Route; got != "/m/2fa/verify" {
		t.Errorf("Route: got %q want %q", got, "/m/2fa/verify")
	}
}

// TestVerifyHandler_ConsumerReqCtxIsEmptyOnTOTPPath confirms the
// handler does NOT build a request context for the TOTP branch — the
// Verify port doesn't take one. (Pinned so a future refactor that
// inadvertently routes TOTP through the consumer would fail loudly.)
func TestVerifyHandler_ConsumerReqCtxIsEmptyOnTOTPPath(t *testing.T) {
	c := &fakeConsumer{}
	h := newVerifyHandler(t, &fakeVerifier{}, c, &fakeSessions{})
	body := url.Values{"code": {"287082"}}
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("User-Agent", "Mozilla/5.0")
	r.RemoteAddr = "203.0.113.5:51234"
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uuid.New(), Email: "ops"}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if c.calls != 0 {
		t.Fatalf("ConsumeRecovery calls: got %d want 0 (TOTP path)", c.calls)
	}
}

func TestVerifyHandler_ConsumerReqCtx_HandlesEmptyRemoteAddr(t *testing.T) {
	// httptest.NewRequest defaults RemoteAddr but a malformed value
	// must not panic — clientIP returns "" instead.
	c := &fakeConsumer{}
	h := newVerifyHandler(t, &fakeVerifier{}, c, &fakeSessions{})
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", strings.NewReader("code=ABCDE-23456"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = ""
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uuid.New(), Email: "ops"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if c.calls != 1 {
		t.Fatalf("calls: got %d", c.calls)
	}
	if c.lastReqCtx.IP != "" {
		t.Errorf("IP: got %q want empty", c.lastReqCtx.IP)
	}
}

func TestVerifyHandler_ConsumerReqCtx_HandlesBareIP(t *testing.T) {
	// SplitHostPort fails on a bare IP — the handler must return the
	// bare value rather than crash.
	c := &fakeConsumer{}
	h := newVerifyHandler(t, &fakeVerifier{}, c, &fakeSessions{})
	r := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", strings.NewReader("code=ABCDE-23456"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "10.0.0.1"
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uuid.New(), Email: "ops"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if c.lastReqCtx.IP != "10.0.0.1" {
		t.Errorf("IP: got %q want %q", c.lastReqCtx.IP, "10.0.0.1")
	}
}

// TestRegenerateHandler_RegeneratorReceivesRequestContext mirrors the
// verify-handler assertion for the regenerate path.
func TestRegenerateHandler_RegeneratorReceivesRequestContext(t *testing.T) {
	regen := &fakeRegenerator{codes: sampleRegenCodes()}
	h := mastermfa.NewRegenerateHandler(mastermfa.RegenerateHandlerConfig{Regenerator: regen})

	r := httptest.NewRequest(http.MethodPost, "/m/2fa/recovery/regenerate", nil)
	r.Header.Set("User-Agent", "curl/8.0.1")
	r.RemoteAddr = "198.51.100.42:443"
	r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uuid.New(), Email: "ops"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if regen.calls != 1 {
		t.Fatalf("Regenerator calls: got %d want 1", regen.calls)
	}
	if got := regen.lastReqCtx.IP; got != "198.51.100.42" {
		t.Errorf("IP: got %q want %q", got, "198.51.100.42")
	}
	if got := regen.lastReqCtx.UserAgent; got != "curl/8.0.1" {
		t.Errorf("UserAgent: got %q", got)
	}
	if got := regen.lastReqCtx.Route; got != "/m/2fa/recovery/regenerate" {
		t.Errorf("Route: got %q", got)
	}
}
