package inbox

import (
	"time"

	"github.com/google/uuid"
)

// Assignment is an append-only history row in `assignment_history`
// (migration 0092 / F2-03): it records that a user became the
// responsible operator for a conversation at AssignedAt for the given
// Reason. The schema has no `unassigned_at` column — the canonical
// "current leader" query is `ORDER BY assigned_at DESC LIMIT 1` per
// (tenant_id, conversation_id).
type Assignment struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	ConversationID uuid.UUID
	UserID         uuid.UUID
	AssignedAt     time.Time
	Reason         LeadReason
}

// NewAssignment constructs a fresh assignment_history row with a typed
// Reason. Rejects zero UUIDs and invalid reasons so the row cannot
// drift into a constraint-violation state at the database boundary.
func NewAssignment(
	tenantID, conversationID, userID uuid.UUID,
	reason LeadReason,
) (*Assignment, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	if conversationID == uuid.Nil {
		return nil, ErrInvalidContact
	}
	if userID == uuid.Nil {
		return nil, ErrInvalidAssignee
	}
	if !reason.Valid() {
		return nil, ErrInvalidLeadReason
	}
	return &Assignment{
		ID:             uuid.New(),
		TenantID:       tenantID,
		ConversationID: conversationID,
		UserID:         userID,
		AssignedAt:     now(),
		Reason:         reason,
	}, nil
}

// HydrateAssignment rebuilds an Assignment from stored fields without
// running the constructor's invariants. Adapter code uses it after
// SELECTs on `assignment_history`.
func HydrateAssignment(
	id, tenantID, conversationID, userID uuid.UUID,
	assignedAt time.Time,
	reason LeadReason,
) *Assignment {
	return &Assignment{
		ID:             id,
		TenantID:       tenantID,
		ConversationID: conversationID,
		UserID:         userID,
		AssignedAt:     assignedAt,
		Reason:         reason,
	}
}
