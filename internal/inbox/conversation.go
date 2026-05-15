package inbox

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// ConversationState is the lifecycle state of a Conversation. The
// CHECK constraint on migration 0088 lists exactly these values.
type ConversationState string

const (
	// ConversationStateOpen means the conversation is active — new
	// messages can be recorded and a user can be assigned.
	ConversationStateOpen ConversationState = "open"
	// ConversationStateClosed means the conversation has been wrapped
	// up. Reopening it lifts the lock.
	ConversationStateClosed ConversationState = "closed"
)

// now is overridable so tests can pin timestamps deterministically. The
// var lives in this file because it is the file inbox callers touch
// first; both message.go and assignment.go reference it.
var now = func() time.Time { return time.Now().UTC() }

// Conversation is the aggregate root for a chat thread between the
// tenant and a contact. Identity is the UUID; equality is by ID.
// Tenancy and the underlying contact are fixed at construction.
//
// State transitions are guarded by the methods on this type — direct
// field mutation by callers is technically possible but breaks the
// invariants we care about (LastMessageAt, AssignedUserID coherence
// with State). Treat Conversation as immutable-from-outside.
type Conversation struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	ContactID      uuid.UUID
	Channel        string
	State          ConversationState
	AssignedUserID *uuid.UUID
	LastMessageAt  time.Time
	CreatedAt      time.Time
	// history is the in-memory projection of `assignment_history` rows
	// loaded by the adapter, oldest-first. Domain methods Lead() /
	// History() / AssignLead() operate on this slice.
	history []*Assignment
}

// NewConversation constructs a fresh, open conversation. AssignedUserID
// starts nil; LastMessageAt starts at zero. The constructor lower-cases
// channel and rejects an empty trimmed name.
func NewConversation(tenantID, contactID uuid.UUID, channel string) (*Conversation, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	if contactID == uuid.Nil {
		return nil, ErrInvalidContact
	}
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		return nil, ErrInvalidChannel
	}
	t := now()
	return &Conversation{
		ID:        uuid.New(),
		TenantID:  tenantID,
		ContactID: contactID,
		Channel:   channel,
		State:     ConversationStateOpen,
		CreatedAt: t,
	}, nil
}

// HydrateConversation rebuilds a Conversation from stored fields
// without running the constructor's invariants. Adapter code uses it
// to materialise rows. Domain code MUST use NewConversation.
func HydrateConversation(id, tenantID, contactID uuid.UUID, channel string,
	state ConversationState, assignedUserID *uuid.UUID, lastMessageAt, createdAt time.Time) *Conversation {
	return &Conversation{
		ID:             id,
		TenantID:       tenantID,
		ContactID:      contactID,
		Channel:        channel,
		State:          state,
		AssignedUserID: assignedUserID,
		LastMessageAt:  lastMessageAt,
		CreatedAt:      createdAt,
	}
}

// AssignTo is the F2-07 leader-attribution mutator: it builds a fresh
// assignment_history row via NewAssignment, appends it to the in-memory
// history slice, and refreshes AssignedUserID. The returned Assignment
// is what the caller MUST hand to AssignmentRepository.AppendHistory
// for persistence.
//
// Errors mirror NewAssignment plus ErrConversationClosed when the
// conversation has been closed.
func (c *Conversation) AssignTo(userID uuid.UUID, reason LeadReason) (*Assignment, error) {
	if c.State != ConversationStateOpen {
		return nil, ErrConversationClosed
	}
	a, err := NewAssignment(c.TenantID, c.ID, userID, reason)
	if err != nil {
		return nil, err
	}
	c.history = append(c.history, a)
	uid := userID
	c.AssignedUserID = &uid
	return a, nil
}

// AssignLead is a transitional alias for AssignTo kept so the F2-07
// leader test suite (added in PR #117) keeps compiling without
// falling under Rule 3 test edits. Prefer AssignTo in new code; this
// shim will be removed under a separate Rule 3 ACK.
func (c *Conversation) AssignLead(userID uuid.UUID, reason LeadReason) (*Assignment, error) {
	return c.AssignTo(userID, reason)
}

// Lead returns the current leader of the conversation: the most recent
// row in the in-memory history. Returns nil when no leader has ever
// been recorded (history empty). Callers MUST NOT mutate the returned
// pointer — Lead() yields a view, not ownership.
func (c *Conversation) Lead() *Assignment {
	if len(c.history) == 0 {
		return nil
	}
	return c.history[len(c.history)-1]
}

// History returns the in-memory assignment_history projection,
// oldest-first. The slice header is a copy so callers cannot mutate
// the conversation's internal storage by appending to it. Returns
// a nil slice when the conversation has no recorded history.
func (c *Conversation) History() []*Assignment {
	if len(c.history) == 0 {
		return nil
	}
	out := make([]*Assignment, len(c.history))
	copy(out, c.history)
	return out
}

// SetHistory replaces the in-memory history slice with the rows
// loaded from `assignment_history` by the adapter. The caller MUST
// pass rows ordered oldest-first; Lead() relies on the last element
// being the most recent assignment. This is a hydration hook for
// adapter code — domain callers should use AssignLead instead.
func (c *Conversation) SetHistory(rows []*Assignment) {
	if len(rows) == 0 {
		c.history = nil
		return
	}
	c.history = make([]*Assignment, len(rows))
	copy(c.history, rows)
	// Sync the denormalised current-leader field so legacy callers
	// stay coherent. The latest row's user wins.
	uid := rows[len(rows)-1].UserID
	c.AssignedUserID = &uid
}

// Close marks the conversation as closed. Idempotent: closing an
// already-closed conversation is a no-op.
func (c *Conversation) Close() {
	c.State = ConversationStateClosed
}

// Reopen lifts a closed conversation back to open. Returns
// ErrConversationAlreadyOpen if the conversation is already open so
// the caller can tell "no-op" from "transition applied".
func (c *Conversation) Reopen() error {
	if c.State == ConversationStateOpen {
		return ErrConversationAlreadyOpen
	}
	c.State = ConversationStateOpen
	return nil
}

// RecordMessage updates the conversation's bookkeeping (LastMessageAt
// in particular) to reflect that m belongs to it. Returns
// ErrConversationClosed if the conversation has been closed and
// ErrConversationMismatch if m.ConversationID does not match.
//
// LastMessageAt is bumped to m.CreatedAt only when it is strictly
// greater than the current LastMessageAt — out-of-order delivery from
// the carrier MUST NOT make the inbox scroll backwards in time.
func (c *Conversation) RecordMessage(m *Message) error {
	if m == nil {
		return ErrConversationMismatch
	}
	if c.State != ConversationStateOpen {
		return ErrConversationClosed
	}
	if m.ConversationID != c.ID {
		return ErrConversationMismatch
	}
	if m.CreatedAt.After(c.LastMessageAt) {
		c.LastMessageAt = m.CreatedAt
	}
	return nil
}
