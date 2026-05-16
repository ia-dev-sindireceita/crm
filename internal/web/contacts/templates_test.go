package contacts

// Internal-package tests for the template helpers. The test file shares
// the `contacts` package so the unexported funcs are reachable without
// changing visibility.

import (
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/contacts"
)

func TestLinkReasonLabel(t *testing.T) {
	t.Parallel()
	cases := map[contacts.LinkReason]string{
		contacts.LinkReasonPhone:      "Telefone",
		contacts.LinkReasonEmail:      "E-mail",
		contacts.LinkReasonExternalID: "ID externo",
		contacts.LinkReasonManual:     "Manual",
		contacts.LinkReason("???"):    "???",
	}
	for reason, want := range cases {
		if got := linkReasonLabel(reason); got != want {
			t.Errorf("linkReasonLabel(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestFormatTime(t *testing.T) {
	t.Parallel()
	if got := formatTime(time.Time{}); got != "" {
		t.Errorf("zero time = %q, want empty", got)
	}
	in := time.Date(2026, 5, 16, 12, 34, 56, 0, time.UTC)
	if got, want := formatTime(in), "2026-05-16 12:34 UTC"; got != want {
		t.Errorf("formatTime = %q, want %q", got, want)
	}
}
