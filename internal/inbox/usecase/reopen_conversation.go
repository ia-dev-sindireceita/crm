package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// ReopenConversation is the write-side use case behind the inbox
// "Reabrir conversa" action (SIN-65473): an operator lifts a closed
// conversation back to open so it can receive messages and be transferred
// again. It is the inverse of CloseConversation and shares its ports.
//
// Invariants:
//
//  1. Conversation isolation — the conversation MUST exist under the tenant
//     scope; an unknown / RLS-hidden id collapses to ErrNotFound (the IDOR
//     guard).
//  2. State coherence — the Conversation aggregate owns the transition
//     (Conversation.Reopen), and the denormalised conversation.state column
//     is updated through the state-store port.
//
// Reopening is idempotent: reopening an already-open conversation succeeds
// with AlreadyOpen=true and skips the redundant write (the aggregate's
// Reopen returns ErrConversationAlreadyOpen, which the use case folds into
// the idempotent result rather than surfacing as a failure).
//
// No SQL or transport lives here — the use case talks only to ports.
type ReopenConversation struct {
	conversations conversationReader
	state         conversationStateWriter
}

// ReopenConversationInput is the use-case argument. Both ids are required.
type ReopenConversationInput struct {
	TenantID       uuid.UUID
	ConversationID uuid.UUID
}

// ReopenConversationResult reports the post-transition state. AlreadyOpen
// is true when the conversation was already open on entry.
type ReopenConversationResult struct {
	AlreadyOpen bool
}

// NewReopenConversation wires the use case. Both ports are required.
func NewReopenConversation(conversations conversationReader, state conversationStateWriter) (*ReopenConversation, error) {
	if conversations == nil {
		return nil, errors.New("inbox/usecase: conversation reader must not be nil")
	}
	if state == nil {
		return nil, errors.New("inbox/usecase: conversation state writer must not be nil")
	}
	return &ReopenConversation{conversations: conversations, state: state}, nil
}

// MustNewReopenConversation is the panic-on-error variant for the
// composition root.
func MustNewReopenConversation(conversations conversationReader, state conversationStateWriter) *ReopenConversation {
	u, err := NewReopenConversation(conversations, state)
	if err != nil {
		panic(err)
	}
	return u
}

// Execute runs the reopen pipeline: validate → load under tenant scope →
// apply the domain transition → persist the open state (skipped on the
// already-open no-op).
func (u *ReopenConversation) Execute(ctx context.Context, in ReopenConversationInput) (ReopenConversationResult, error) {
	if in.TenantID == uuid.Nil {
		return ReopenConversationResult{}, inbox.ErrInvalidTenant
	}
	if in.ConversationID == uuid.Nil {
		return ReopenConversationResult{}, inbox.ErrNotFound
	}

	conv, err := u.conversations.GetConversation(ctx, in.TenantID, in.ConversationID)
	if err != nil {
		return ReopenConversationResult{}, err
	}

	if err := conv.Reopen(); err != nil {
		if errors.Is(err, inbox.ErrConversationAlreadyOpen) {
			// Idempotent no-op: nothing to persist, the column is already open.
			return ReopenConversationResult{AlreadyOpen: true}, nil
		}
		return ReopenConversationResult{}, err
	}

	if err := u.state.SetConversationState(ctx, in.TenantID, in.ConversationID, inbox.ConversationStateOpen); err != nil {
		return ReopenConversationResult{}, err
	}

	return ReopenConversationResult{}, nil
}
