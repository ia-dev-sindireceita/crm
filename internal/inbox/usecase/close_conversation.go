package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// conversationStateWriter persists a conversation's lifecycle state. It is
// the write seam the CloseConversation use case needs and is satisfied
// structurally by the postgres inbox Store (SetConversationState). Declaring
// it here (accept-narrow) keeps the use case's dependency surface to exactly
// the one method it exercises.
type conversationStateWriter interface {
	SetConversationState(ctx context.Context, tenantID, conversationID uuid.UUID, state inbox.ConversationState) error
}

// CloseConversation is the write-side use case behind the "Encerrar conversa"
// action (SIN-65471 AC#4). It loads the conversation under the tenant scope
// (RLS-hidden / unknown ids collapse to ErrNotFound — the conversationID IDOR
// guard), applies the domain transition via Conversation.Close (idempotent —
// closing an already-closed thread is a no-op), and persists the new state
// through the state-writer port.
//
// No SQL or transport lives here — the use case talks only to ports, so the
// close transition is testable without a database and the same code path
// serves the HTMX handler and any future API caller.
type CloseConversation struct {
	conversations conversationReader
	states        conversationStateWriter
}

// NewCloseConversation wires the use case. Both ports are required; a nil
// port is a programming error caught here.
func NewCloseConversation(conversations conversationReader, states conversationStateWriter) (*CloseConversation, error) {
	if conversations == nil {
		return nil, errors.New("inbox/usecase: close conversation reader must not be nil")
	}
	if states == nil {
		return nil, errors.New("inbox/usecase: close conversation state writer must not be nil")
	}
	return &CloseConversation{conversations: conversations, states: states}, nil
}

// MustNewCloseConversation is the panic-on-error variant for the composition
// root.
func MustNewCloseConversation(conversations conversationReader, states conversationStateWriter) *CloseConversation {
	u, err := NewCloseConversation(conversations, states)
	if err != nil {
		panic(err)
	}
	return u
}

// CloseConversationInput is the use-case argument.
type CloseConversationInput struct {
	TenantID       uuid.UUID
	ConversationID uuid.UUID
}

// CloseConversationResult reports the post-transition state so the handler can
// re-render the conversation actions without a second read.
type CloseConversationResult struct {
	State inbox.ConversationState
}

// Execute runs the close pipeline: validate → load + tenant guard → domain
// transition → persist.
func (u *CloseConversation) Execute(ctx context.Context, in CloseConversationInput) (CloseConversationResult, error) {
	if in.TenantID == uuid.Nil {
		return CloseConversationResult{}, inbox.ErrInvalidTenant
	}
	if in.ConversationID == uuid.Nil {
		return CloseConversationResult{}, ErrNotFound
	}

	conv, err := u.conversations.GetConversation(ctx, in.TenantID, in.ConversationID)
	if err != nil {
		return CloseConversationResult{}, err
	}

	// Idempotent domain transition: closing an already-closed conversation is
	// a no-op, but we still persist so the operation converges even if a prior
	// attempt advanced the aggregate without persisting.
	conv.Close()

	if err := u.states.SetConversationState(ctx, in.TenantID, in.ConversationID, conv.State); err != nil {
		return CloseConversationResult{}, err
	}
	return CloseConversationResult{State: conv.State}, nil
}
