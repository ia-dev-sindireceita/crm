package inbox_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

func TestNewAssignment_Validates(t *testing.T) {
	tenant := uuid.New()
	conv := uuid.New()
	user := uuid.New()

	tests := []struct {
		name           string
		tenant         uuid.UUID
		conversationID uuid.UUID
		userID         uuid.UUID
		reason         inbox.LeadReason
		wantErr        error
	}{
		{"zero tenant", uuid.Nil, conv, user, inbox.LeadReasonLead, inbox.ErrInvalidTenant},
		{"zero conversation", tenant, uuid.Nil, user, inbox.LeadReasonLead, inbox.ErrInvalidContact},
		{"zero user", tenant, conv, uuid.Nil, inbox.LeadReasonLead, inbox.ErrInvalidAssignee},
		{"empty reason", tenant, conv, user, inbox.LeadReason(""), inbox.ErrInvalidLeadReason},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := inbox.NewAssignment(tc.tenant, tc.conversationID, tc.userID, tc.reason)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestNewAssignment_PopulatesFields(t *testing.T) {
	tenant := uuid.New()
	conv := uuid.New()
	user := uuid.New()
	a, err := inbox.NewAssignment(tenant, conv, user, inbox.LeadReasonLead)
	if err != nil {
		t.Fatalf("NewAssignment: %v", err)
	}
	if a.ID == uuid.Nil {
		t.Error("ID = uuid.Nil")
	}
	if a.AssignedAt.IsZero() {
		t.Error("AssignedAt is zero")
	}
	if a.Reason != inbox.LeadReasonLead {
		t.Errorf("Reason = %q, want %q", a.Reason, inbox.LeadReasonLead)
	}
}

func TestHydrateAssignment_Roundtrip(t *testing.T) {
	id := uuid.New()
	tenant := uuid.New()
	conv := uuid.New()
	user := uuid.New()
	assigned := time.Now().UTC().Truncate(time.Second)
	a := inbox.HydrateAssignment(id, tenant, conv, user, assigned, inbox.LeadReasonReassign)
	if a.ID != id || a.TenantID != tenant || a.ConversationID != conv || a.UserID != user ||
		!a.AssignedAt.Equal(assigned) || a.Reason != inbox.LeadReasonReassign {
		t.Errorf("Hydrate mismatch: %+v", a)
	}
}
