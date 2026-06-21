package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// unassignLedger is the narrow append-only seam UnassignConversation
// needs: record the explicit unassign event row in assignment_history.
// Satisfied by the postgres inbox Store (AppendUnassign). Declared here
// (accept-narrow) so the use case depends on one method rather than the
// full AssignmentRepository.
type unassignLedger interface {
	AppendUnassign(ctx context.Context, tenantID, conversationID uuid.UUID) (*inbox.Assignment, error)
}

// UnassignConversation returns a conversation to the Não atribuído state:
// it clears the denormalised current-lead column (read-model) and
// records an explicit unassign event in the append-only
// assignment_history ledger (SIN-65480). It is the "Transferir para Não
// atribuído" path deferred from SIN-65473 — the ledger could not
// represent "assigned to nobody" until migration 0124 made user_id
// nullable and added the 'unassign' reason.
//
// Security posture mirrors AssignConversation: the conversation is
// loaded under the tenant scope first, so an RLS-hidden / unknown id
// collapses to ErrNotFound (IDOR guard) before any write; the closed
// lifecycle gate (Conversation.Unassign → ErrConversationClosed) blocks
// leadership changes on a closed conversation.
type UnassignConversation struct {
	conversations conversationReader
	ledger        unassignLedger
	leadCache     AssignmentClearer
}

// NewUnassignConversation wires the use case. Every collaborator is
// required: a nil dependency is a programming error caught here rather
// than as a nil-deref at request time.
func NewUnassignConversation(conversations conversationReader, ledger unassignLedger, leadCache AssignmentClearer) (*UnassignConversation, error) {
	if conversations == nil {
		return nil, errors.New("inbox/usecase: unassign conversations reader must not be nil")
	}
	if ledger == nil {
		return nil, errors.New("inbox/usecase: unassign ledger must not be nil")
	}
	if leadCache == nil {
		return nil, errors.New("inbox/usecase: unassign lead cache must not be nil")
	}
	return &UnassignConversation{
		conversations: conversations,
		ledger:        ledger,
		leadCache:     leadCache,
	}, nil
}

// MustNewUnassignConversation is the panic-on-error variant for the
// composition root.
func MustNewUnassignConversation(conversations conversationReader, ledger unassignLedger, leadCache AssignmentClearer) *UnassignConversation {
	u, err := NewUnassignConversation(conversations, ledger, leadCache)
	if err != nil {
		panic(err)
	}
	return u
}

// UnassignConversationInput is the use-case argument.
type UnassignConversationInput struct {
	TenantID       uuid.UUID
	ConversationID uuid.UUID
}

// UnassignConversationResult reports the outcome. AlreadyUnassigned is
// true when the conversation already had no leader — an idempotent no-op
// that records no ledger row (so repeated clicks do not pollute the
// audit trail).
type UnassignConversationResult struct {
	AlreadyUnassigned bool
}

// Execute runs the unassign pipeline: validate → load under tenant scope
// → apply the domain transition (closed gate) → on a real transition,
// append the unassign event then clear the denormalised lead.
func (u *UnassignConversation) Execute(ctx context.Context, in UnassignConversationInput) (UnassignConversationResult, error) {
	if in.TenantID == uuid.Nil {
		return UnassignConversationResult{}, inbox.ErrInvalidTenant
	}
	if in.ConversationID == uuid.Nil {
		return UnassignConversationResult{}, ErrNotFound
	}

	conv, err := u.conversations.GetConversation(ctx, in.TenantID, in.ConversationID)
	if err != nil {
		return UnassignConversationResult{}, err
	}

	alreadyUnassigned := conv.AssignedUserID == nil

	// Domain transition: gates on the open lifecycle (closed →
	// ErrConversationClosed) and clears the in-memory leader. Run BEFORE
	// the no-op check so a closed conversation is rejected even when it is
	// already unassigned, mirroring AssignTo's close gate.
	if err := conv.Unassign(); err != nil {
		return UnassignConversationResult{}, err
	}

	// Idempotent no-op: an already-unassigned (open) conversation needs no
	// ledger row and no cache write. Appending a redundant unassign event
	// on every click would bloat the append-only audit trail with noise.
	if alreadyUnassigned {
		return UnassignConversationResult{AlreadyUnassigned: true}, nil
	}

	// Record the explicit unassign event (audit source of truth) BEFORE
	// clearing the denormalised cache — same ledger-then-cache ordering as
	// AssignConversation, so the cached lead can never be cleared without
	// its matching audit row. Both steps are idempotent on retry: a second
	// unassign event is harmless and clearing an already-cleared lead is a
	// no-op write.
	if _, err := u.ledger.AppendUnassign(ctx, in.TenantID, in.ConversationID); err != nil {
		return UnassignConversationResult{}, err
	}
	if err := u.leadCache.ClearAssignment(ctx, in.TenantID, in.ConversationID); err != nil {
		return UnassignConversationResult{}, err
	}

	return UnassignConversationResult{}, nil
}
