package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// CloseConversation is the write-side use case behind the inbox
// "Encerrar conversa" action (SIN-65473): an operator wraps up a
// conversation, flipping its lifecycle state to closed. Closing is the
// gate the assignment path already honours — AssignTo / RecordMessage
// reject a closed conversation with ErrConversationClosed — so once
// closed a conversation can no longer be transferred or replied to until
// it is reopened.
//
// It owns two invariants the web layer MUST NOT be trusted to enforce:
//
//  1. Conversation isolation — the conversation MUST exist under the
//     tenant scope; an unknown / RLS-hidden id collapses to ErrNotFound
//     (the conversationID IDOR guard).
//  2. State coherence — the Conversation aggregate owns the transition
//     (Conversation.Close), and the denormalised conversation.state column
//     is updated through the state-store port so the list read-model and
//     the metrics open/closed split stay coherent.
//
// Closing is idempotent: closing an already-closed conversation succeeds
// with AlreadyClosed=true and re-persists the closed state (a no-op write)
// rather than erroring, so a double-click or a retry is safe.
//
// No SQL or transport lives here — the use case talks only to ports.
type CloseConversation struct {
	conversations conversationReader
	state         conversationStateWriter
}

// conversationStateWriter is the narrow write seam for the conversation
// lifecycle column. Satisfied by the postgres inbox Store
// (SetConversationState). Declared here and reused by ReopenConversation.
type conversationStateWriter interface {
	SetConversationState(ctx context.Context, tenantID, conversationID uuid.UUID, state inbox.ConversationState) error
}

// CloseConversationInput is the use-case argument. Both ids are required.
type CloseConversationInput struct {
	TenantID       uuid.UUID
	ConversationID uuid.UUID
}

// CloseConversationResult reports the post-transition state. AlreadyClosed
// is true when the conversation was already closed on entry, so the handler
// can render an idempotent confirmation instead of implying a fresh
// transition.
type CloseConversationResult struct {
	AlreadyClosed bool
}

// NewCloseConversation wires the use case. Both ports are required.
func NewCloseConversation(conversations conversationReader, state conversationStateWriter) (*CloseConversation, error) {
	if conversations == nil {
		return nil, errors.New("inbox/usecase: conversation reader must not be nil")
	}
	if state == nil {
		return nil, errors.New("inbox/usecase: conversation state writer must not be nil")
	}
	return &CloseConversation{conversations: conversations, state: state}, nil
}

// MustNewCloseConversation is the panic-on-error variant for the
// composition root.
func MustNewCloseConversation(conversations conversationReader, state conversationStateWriter) *CloseConversation {
	u, err := NewCloseConversation(conversations, state)
	if err != nil {
		panic(err)
	}
	return u
}

// Execute runs the close pipeline: validate → load under tenant scope →
// apply the domain transition → persist the closed state.
func (u *CloseConversation) Execute(ctx context.Context, in CloseConversationInput) (CloseConversationResult, error) {
	if in.TenantID == uuid.Nil {
		return CloseConversationResult{}, inbox.ErrInvalidTenant
	}
	if in.ConversationID == uuid.Nil {
		return CloseConversationResult{}, inbox.ErrNotFound
	}

	conv, err := u.conversations.GetConversation(ctx, in.TenantID, in.ConversationID)
	if err != nil {
		return CloseConversationResult{}, err
	}

	alreadyClosed := conv.State == inbox.ConversationStateClosed

	// Close is idempotent on the aggregate; we still persist below so a
	// drifted read-model column is reconciled even on the no-op path.
	conv.Close()

	if err := u.state.SetConversationState(ctx, in.TenantID, in.ConversationID, inbox.ConversationStateClosed); err != nil {
		return CloseConversationResult{}, err
	}

	return CloseConversationResult{AlreadyClosed: alreadyClosed}, nil
}
