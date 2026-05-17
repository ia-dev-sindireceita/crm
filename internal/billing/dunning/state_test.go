package dunning_test

import (
	"testing"

	"github.com/pericles-luz/crm/internal/billing/dunning"
)

func TestState_Severity_Ordering(t *testing.T) {
	// Ordered list — Severity() must agree with this sequence.
	ordered := []dunning.State{
		dunning.StateCurrent,
		dunning.StateWarn,
		dunning.StateSuspendedOutbound,
		dunning.StateSuspendedFull,
		dunning.StateCancelled,
	}
	for i, s := range ordered {
		if got := s.Severity(); got != i {
			t.Errorf("Severity(%s) = %d, want %d", s, got, i)
		}
	}
}

func TestState_Severity_Unknown(t *testing.T) {
	if got := dunning.State("bogus").Severity(); got != -1 {
		t.Errorf("Severity(bogus) = %d, want -1", got)
	}
}

func TestState_IsTerminal(t *testing.T) {
	cases := map[dunning.State]bool{
		dunning.StateCurrent:           false,
		dunning.StateWarn:              false,
		dunning.StateSuspendedOutbound: false,
		dunning.StateSuspendedFull:     false,
		dunning.StateCancelled:         true,
	}
	for s, want := range cases {
		if got := s.IsTerminal(); got != want {
			t.Errorf("IsTerminal(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestState_IsKnown(t *testing.T) {
	known := []dunning.State{
		dunning.StateCurrent,
		dunning.StateWarn,
		dunning.StateSuspendedOutbound,
		dunning.StateSuspendedFull,
		dunning.StateCancelled,
	}
	for _, s := range known {
		if !s.IsKnown() {
			t.Errorf("IsKnown(%s) = false, want true", s)
		}
	}
	if dunning.State("bogus").IsKnown() {
		t.Errorf("IsKnown(bogus) = true, want false")
	}
}
