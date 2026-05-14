package inbox

import (
	"time"

	"github.com/google/uuid"
)

// Assignment is an append-only history row: it records that a user was
// the responsible operator for a conversation during a given interval.
// AssignedAt is set at construction; UnassignedAt is filled when the
// next assignment is recorded (or when the conversation closes).
//
// The aggregate root for assignment history is the Conversation, but
// the rows live in their own table because the canonical inbox query
// is "who is currently assigned" — a derived view over the latest
// row with UnassignedAt IS NULL.
type Assignment struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	ConversationID uuid.UUID
	UserID         uuid.UUID
	AssignedAt     time.Time
	UnassignedAt   *time.Time
}

// NewAssignment constructs an open-ended assignment row (UnassignedAt
// nil). Rejects zero tenant / conversation / user ids so the row
// cannot drift into a NULL-FK state at the database boundary.
func NewAssignment(tenantID, conversationID, userID uuid.UUID) (*Assignment, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	if conversationID == uuid.Nil {
		return nil, ErrInvalidContact
	}
	if userID == uuid.Nil {
		return nil, ErrInvalidAssignee
	}
	return &Assignment{
		ID:             uuid.New(),
		TenantID:       tenantID,
		ConversationID: conversationID,
		UserID:         userID,
		AssignedAt:     now(),
	}, nil
}

// HydrateAssignment rebuilds an Assignment from stored fields without
// running the constructor's invariants. Adapter code uses it to
// materialise rows.
func HydrateAssignment(id, tenantID, conversationID, userID uuid.UUID,
	assignedAt time.Time, unassignedAt *time.Time) *Assignment {
	return &Assignment{
		ID:             id,
		TenantID:       tenantID,
		ConversationID: conversationID,
		UserID:         userID,
		AssignedAt:     assignedAt,
		UnassignedAt:   unassignedAt,
	}
}

// MarkUnassigned closes the assignment interval at t. Idempotent on
// the value: a second call with a different t is rejected so the
// history row stays immutable once closed.
func (a *Assignment) MarkUnassigned(t time.Time) error {
	if a.UnassignedAt != nil {
		if a.UnassignedAt.Equal(t) {
			return nil
		}
		return ErrConversationMismatch
	}
	tt := t
	a.UnassignedAt = &tt
	return nil
}
