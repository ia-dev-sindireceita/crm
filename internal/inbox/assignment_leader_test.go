package inbox_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

var leaderFixedTime = time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)

func TestNewLeaderAssignment_Validates(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	conv := uuid.New()
	user := uuid.New()

	tests := []struct {
		name    string
		tenant  uuid.UUID
		conv    uuid.UUID
		user    uuid.UUID
		reason  inbox.LeadReason
		wantErr error
	}{
		{"zero tenant", uuid.Nil, conv, user, inbox.LeadReasonLead, inbox.ErrInvalidTenant},
		{"zero conversation", tenant, uuid.Nil, user, inbox.LeadReasonLead, inbox.ErrInvalidContact},
		{"zero user", tenant, conv, uuid.Nil, inbox.LeadReasonLead, inbox.ErrInvalidAssignee},
		{"empty reason", tenant, conv, user, inbox.LeadReason(""), inbox.ErrInvalidLeadReason},
		{"unknown reason", tenant, conv, user, inbox.LeadReason("auto"), inbox.ErrInvalidLeadReason},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := inbox.NewLeaderAssignment(tc.tenant, tc.conv, tc.user, tc.reason)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestNewLeaderAssignment_PopulatesFields(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	conv := uuid.New()
	user := uuid.New()
	a, err := inbox.NewLeaderAssignment(tenant, conv, user, inbox.LeadReasonManual)
	if err != nil {
		t.Fatalf("NewLeaderAssignment: %v", err)
	}
	if a.ID == uuid.Nil {
		t.Error("ID = uuid.Nil")
	}
	if a.TenantID != tenant || a.ConversationID != conv || a.UserID != user {
		t.Errorf("identity fields not propagated: %+v", a)
	}
	if a.Reason != inbox.LeadReasonManual {
		t.Errorf("Reason = %q, want %q", a.Reason, inbox.LeadReasonManual)
	}
	if a.AssignedAt.IsZero() {
		t.Error("AssignedAt is zero")
	}
	if a.UnassignedAt != nil {
		t.Errorf("UnassignedAt = %v, want nil", a.UnassignedAt)
	}
}

func TestHydrateLeaderAssignment_Roundtrip(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	tenant := uuid.New()
	conv := uuid.New()
	user := uuid.New()
	a := inbox.HydrateLeaderAssignment(id, tenant, conv, user, leaderFixedTime, inbox.LeadReasonReassign)
	if a.ID != id || a.TenantID != tenant || a.ConversationID != conv || a.UserID != user {
		t.Errorf("identity mismatch: %+v", a)
	}
	if !a.AssignedAt.Equal(leaderFixedTime) {
		t.Errorf("AssignedAt = %v, want %v", a.AssignedAt, leaderFixedTime)
	}
	if a.Reason != inbox.LeadReasonReassign {
		t.Errorf("Reason = %q, want %q", a.Reason, inbox.LeadReasonReassign)
	}
	if a.UnassignedAt != nil {
		t.Errorf("UnassignedAt = %v, want nil (F2-03 schema has no such column)", a.UnassignedAt)
	}
}
