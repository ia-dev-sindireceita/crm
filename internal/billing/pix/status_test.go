package pix_test

import (
	"testing"

	"github.com/pericles-luz/crm/internal/billing/pix"
)

func TestStatus_IsTerminal(t *testing.T) {
	cases := map[pix.Status]bool{
		pix.StatusPending:   false,
		pix.StatusPaid:      true,
		pix.StatusExpired:   true,
		pix.StatusCancelled: true,
	}
	for s, want := range cases {
		if got := s.IsTerminal(); got != want {
			t.Errorf("IsTerminal(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestStatus_IsTerminal_Unknown(t *testing.T) {
	if pix.Status("bogus").IsTerminal() {
		t.Errorf("IsTerminal(bogus) = true, want false")
	}
}

func TestStatus_IsKnown(t *testing.T) {
	known := []pix.Status{
		pix.StatusPending,
		pix.StatusPaid,
		pix.StatusExpired,
		pix.StatusCancelled,
	}
	for _, s := range known {
		if !s.IsKnown() {
			t.Errorf("IsKnown(%s) = false, want true", s)
		}
	}
	if pix.Status("bogus").IsKnown() {
		t.Errorf("IsKnown(bogus) = true, want false")
	}
	if pix.Status("").IsKnown() {
		t.Errorf("IsKnown(empty) = true, want false")
	}
}

func TestWebhookEventType_IsKnown(t *testing.T) {
	known := []pix.WebhookEventType{
		pix.WebhookEventPaid,
		pix.WebhookEventExpired,
		pix.WebhookEventCancelled,
	}
	for _, et := range known {
		if !et.IsKnown() {
			t.Errorf("IsKnown(%s) = false, want true", et)
		}
	}
	if pix.WebhookEventType("bogus").IsKnown() {
		t.Errorf("IsKnown(bogus) = true, want false")
	}
	if pix.WebhookEventType("").IsKnown() {
		t.Errorf("IsKnown(empty) = true, want false")
	}
}
