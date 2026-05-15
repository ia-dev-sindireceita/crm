package inbox

import (
	"time"

	"github.com/google/uuid"
)

// Assignment is an append-only history row in `assignment_history`
// (migration 0092 / F2-03): it records that a user became the
// responsible operator for a conversation at AssignedAt for the given
// Reason. There is no UnassignedAt column — the canonical "current
// leader" query is `ORDER BY assigned_at DESC LIMIT 1` per
// (tenant_id, conversation_id).
//
// UnassignedAt is preserved on the in-memory value for Fase 1
// callers that still expect it; F2-07 stops persisting it and the
// field will be removed once the Rule-3 refactor lands.
type Assignment struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	ConversationID uuid.UUID
	UserID         uuid.UUID
	AssignedAt     time.Time
	UnassignedAt   *time.Time
	Reason         LeadReason
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

// NewLeaderAssignment is the F2-07 constructor: it builds a fresh
// assignment_history row with a typed Reason. Rejects zero UUIDs and
// invalid reasons so the row cannot drift into a constraint-violation
// state at the database boundary.
func NewLeaderAssignment(
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

// HydrateLeaderAssignment is the F2-07 hydrator: it rebuilds a row
// from `assignment_history` without running constructor invariants.
// AdaptER code uses it after SELECTs. UnassignedAt is not part of the
// F2-03 schema; the field stays on the struct for Fase 1 callers and
// is left nil here.
func HydrateLeaderAssignment(
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
