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

// AssignTo records that userID is now the responsible operator for
// this conversation. Returns ErrConversationClosed if the conversation
// is not open and ErrInvalidAssignee if userID is uuid.Nil.
//
// The change is in-memory; the caller is responsible for persisting it
// (typically via a Repository.SaveConversation or an Assignment row).
func (c *Conversation) AssignTo(userID uuid.UUID) error {
	if c.State != ConversationStateOpen {
		return ErrConversationClosed
	}
	if userID == uuid.Nil {
		return ErrInvalidAssignee
	}
	uid := userID
	c.AssignedUserID = &uid
	return nil
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
