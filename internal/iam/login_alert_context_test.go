package iam

// SIN-62379 — master-lockout Slack alert carries the operational
// context fields ADR 0074 §6 mandates: actor_email (master only,
// unmasked), ip, user_agent, route. CAVEAT-2 of the SIN-62343
// security review.

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// TestLogin_MasterLockoutAlert_CarriesContextFields drives a master
// service to its lockout trip and asserts every ADR 0074 §6 field
// appears in the Notify payload.
func TestLogin_MasterLockoutAlert_CarriesContextFields(t *testing.T) {
	t.Parallel()
	svc, _, _ := newServiceForTest(t)
	svc.Lockouts = newInMemoryLockouts()
	svc.Limiter = newInMemoryLimiter()
	svc.LoginPolicy = masterPolicy(t)
	alerter := &recordingAlerter{}
	svc.Alerter = alerter

	ctx := context.Background()
	const (
		email = "alice@acme.test"
		ua    = "Mozilla/5.0 (X11) suspicious-actor/1.0"
		route = "/m/login"
	)
	ip := net.IPv4(203, 0, 113, 7)

	// m_login Threshold=5 → six wrong-password attempts trip the lockout.
	for i := 0; i < 6; i++ {
		_, err := svc.Login(ctx, "acme.crm.local", email, "WRONG", ip, ua, route)
		if i < 5 {
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("attempt %d: err=%v want ErrInvalidCredentials", i+1, err)
			}
			continue
		}
		if !errors.Is(err, ErrAccountLocked) {
			t.Fatalf("trip attempt: err=%v want ErrAccountLocked", err)
		}
	}

	if alerter.count() != 1 {
		t.Fatalf("alerter.count() = %d, want 1", alerter.count())
	}
	msg := alerter.messages[0]

	// ADR 0074 §6 mandates these fields on the master-lockout alert.
	wantSubstrings := []string{
		"master account locked",
		"email=" + email,
		"ip=[" + ip.String() + "]",
		"ua=[" + ua + "]",
		"route=[" + route + "]",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(msg, want) {
			t.Errorf("alert message missing %q\nfull message: %s", want, msg)
		}
	}
}

// TestLogin_MasterLockoutAlert_NilIP_RendersEmpty asserts that a nil
// ipAddr (boundary failed to parse RemoteAddr) renders as an empty
// bracket-delimited field instead of fmt's "<nil>" so the operator
// alert reads cleanly.
func TestLogin_MasterLockoutAlert_NilIP_RendersEmpty(t *testing.T) {
	t.Parallel()
	svc, _, _ := newServiceForTest(t)
	svc.Lockouts = newInMemoryLockouts()
	svc.Limiter = newInMemoryLimiter()
	svc.LoginPolicy = masterPolicy(t)
	alerter := &recordingAlerter{}
	svc.Alerter = alerter

	ctx := context.Background()
	for i := 0; i < int(svc.LoginPolicy.Lockout.Threshold)+1; i++ {
		_, _ = svc.Login(ctx, "acme.crm.local", "alice@acme.test", "WRONG", nil, "", "/m/login")
	}
	if alerter.count() != 1 {
		t.Fatalf("alerter.count() = %d, want 1", alerter.count())
	}
	msg := alerter.messages[0]
	if !strings.Contains(msg, "ip=[]") {
		t.Errorf("nil IP rendered unexpectedly: %s", msg)
	}
	if strings.Contains(msg, "<nil>") {
		t.Errorf("alert leaked fmt sentinel <nil>: %s", msg)
	}
}
