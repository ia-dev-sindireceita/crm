package slack

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
)

// SIN-62380 (CAVEAT-3): VerifyLockoutAlerter routes the Slack alert
// for the master 2FA verify lockout via the existing Notifier.

func TestNewVerifyLockoutAlerter_PanicsOnNilNotifier(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil notifier")
		}
	}()
	NewVerifyLockoutAlerter(nil)
}

func TestVerifyLockoutAlerter_AlertVerifyLockout(t *testing.T) {
	cap := newCaptureServer(t)
	notifier := New(cap.srv.URL)
	alerter := NewVerifyLockoutAlerter(notifier)
	uid := uuid.New()
	sid := uuid.New()
	details := mastermfa.VerifyLockoutDetails{
		UserID:    uid,
		SessionID: sid,
		Failures:  5,
		IP:        "203.0.113.5",
		UserAgent: "Mozilla/5.0 (X11; Linux x86_64)",
		Route:     "/m/2fa/verify",
	}
	if err := alerter.AlertVerifyLockout(context.Background(), details); err != nil {
		t.Fatalf("AlertVerifyLockout: %v", err)
	}
	bodies := cap.Bodies()
	if len(bodies) != 1 {
		t.Fatalf("bodies: got %d want 1", len(bodies))
	}
	body := bodies[0]
	for _, want := range []string{
		"master_2fa_verify_lockout",
		uid.String(),
		sid.String(),
		"failures=5",
		`ip=[203.0.113.5]`,
		`user_agent=[Mozilla/5.0 (X11; Linux x86_64)]`,
		`route=[/m/2fa/verify]`,
		":lock:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %q", want, body)
		}
	}
}

func TestVerifyLockoutAlerter_EmptyFieldsRenderAsBlank(t *testing.T) {
	// A missing UA / RemoteAddr renders as an obvious blank rather
	// than disappearing — same convention as MFAAlerter.
	cap := newCaptureServer(t)
	notifier := New(cap.srv.URL)
	alerter := NewVerifyLockoutAlerter(notifier)
	details := mastermfa.VerifyLockoutDetails{
		UserID:    uuid.New(),
		SessionID: uuid.New(),
		Failures:  5,
	}
	if err := alerter.AlertVerifyLockout(context.Background(), details); err != nil {
		t.Fatalf("AlertVerifyLockout: %v", err)
	}
	body := cap.Bodies()[0]
	for _, want := range []string{`ip=[]`, `user_agent=[]`, `route=[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %q", want, body)
		}
	}
}

func TestVerifyLockoutAlerter_EmptyWebhookIsNoop(t *testing.T) {
	// Empty webhook turns the underlying Notifier into a no-op.
	alerter := NewVerifyLockoutAlerter(New(""))
	details := mastermfa.VerifyLockoutDetails{
		UserID:    uuid.New(),
		SessionID: uuid.New(),
		Failures:  5,
	}
	if err := alerter.AlertVerifyLockout(context.Background(), details); err != nil {
		t.Errorf("AlertVerifyLockout: %v", err)
	}
}
