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
		wantErr        error
	}{
		{"zero tenant", uuid.Nil, conv, user, inbox.ErrInvalidTenant},
		{"zero conversation", tenant, uuid.Nil, user, inbox.ErrInvalidContact},
		{"zero user", tenant, conv, uuid.Nil, inbox.ErrInvalidAssignee},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := inbox.NewAssignment(tc.tenant, tc.conversationID, tc.userID)
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
	a, err := inbox.NewAssignment(tenant, conv, user)
	if err != nil {
		t.Fatalf("NewAssignment: %v", err)
	}
	if a.ID == uuid.Nil {
		t.Error("ID = uuid.Nil")
	}
	if a.AssignedAt.IsZero() {
		t.Error("AssignedAt is zero")
	}
	if a.UnassignedAt != nil {
		t.Errorf("UnassignedAt = %v, want nil", a.UnassignedAt)
	}
}

func TestAssignment_MarkUnassigned(t *testing.T) {
	a, err := inbox.NewAssignment(uuid.New(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("NewAssignment: %v", err)
	}
	when := time.Now().UTC().Truncate(time.Second)
	if err := a.MarkUnassigned(when); err != nil {
		t.Fatalf("MarkUnassigned: %v", err)
	}
	if a.UnassignedAt == nil || !a.UnassignedAt.Equal(when) {
		t.Errorf("UnassignedAt = %v, want %v", a.UnassignedAt, when)
	}
	// Idempotent on the same value.
	if err := a.MarkUnassigned(when); err != nil {
		t.Errorf("MarkUnassigned same value err = %v, want nil", err)
	}
	// Different time on a closed assignment is rejected.
	if err := a.MarkUnassigned(when.Add(time.Minute)); err == nil {
		t.Error("MarkUnassigned different time err = nil, want error")
	}
}

func TestHydrateAssignment_Roundtrip(t *testing.T) {
	id := uuid.New()
	tenant := uuid.New()
	conv := uuid.New()
	user := uuid.New()
	assigned := time.Now().UTC().Truncate(time.Second)
	unassigned := assigned.Add(time.Hour)
	a := inbox.HydrateAssignment(id, tenant, conv, user, assigned, &unassigned)
	if a.ID != id || a.TenantID != tenant || a.ConversationID != conv || a.UserID != user ||
		!a.AssignedAt.Equal(assigned) || a.UnassignedAt == nil || !a.UnassignedAt.Equal(unassigned) {
		t.Errorf("Hydrate mismatch: %+v", a)
	}
}
