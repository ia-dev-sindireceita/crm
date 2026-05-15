package inbox_test

import (
	"testing"

	"github.com/pericles-luz/crm/internal/inbox"
)

func TestLeadReason_Valid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reason inbox.LeadReason
		valid  bool
	}{
		{inbox.LeadReasonLead, true},
		{inbox.LeadReasonManual, true},
		{inbox.LeadReasonReassign, true},
		{inbox.LeadReason(""), false},
		{inbox.LeadReason("anything-else"), false},
		{inbox.LeadReason("LEAD"), false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(string(tc.reason), func(t *testing.T) {
			t.Parallel()
			if got := tc.reason.Valid(); got != tc.valid {
				t.Errorf("Valid(%q) = %v, want %v", tc.reason, got, tc.valid)
			}
		})
	}
}
