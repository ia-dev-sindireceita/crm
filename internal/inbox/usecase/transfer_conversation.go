package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// conversationLeadClearer releases a conversation's denormalised lead
// (conversation.assigned_user_id → NULL). It is satisfied structurally by the
// postgres inbox Store (ClearConversationLead). Declaring it here keeps the
// dependency surface narrow and lets the use case be tested without storage.
type conversationLeadClearer interface {
	ClearConversationLead(ctx context.Context, tenantID, conversationID uuid.UUID) error
}

// TransferConversation is the write-side use case behind the
// "Transferir conversa" action (SIN-65471 AC#4): it returns a conversation to
// the unassigned queue (the inbox "fila / não atribuídas") so another
// attendant can pick it up. Re-assigning to a *specific* attendant is already
// served by AssignConversation + the assignment dropdown; transfer is the
// release counterpart — it drops the current lead without naming a successor.
//
// It loads the conversation under the tenant scope first (RLS-hidden / unknown
// ids collapse to ErrNotFound — the conversationID IDOR guard), then clears
// the cached lead column the list read-model projects on. The append-only
// assignment_history ledger is intentionally left untouched (it has no
// nil-user "unassigned" row shape); the audit trail of who *was* assigned
// survives, while the live queue + assignee chip reflect Unassigned.
//
// No SQL or transport lives here — the use case talks only to ports.
type TransferConversation struct {
	conversations conversationReader
	leadClearer   conversationLeadClearer
}

// NewTransferConversation wires the use case. Both ports are required.
func NewTransferConversation(conversations conversationReader, leadClearer conversationLeadClearer) (*TransferConversation, error) {
	if conversations == nil {
		return nil, errors.New("inbox/usecase: transfer conversation reader must not be nil")
	}
	if leadClearer == nil {
		return nil, errors.New("inbox/usecase: transfer conversation lead clearer must not be nil")
	}
	return &TransferConversation{conversations: conversations, leadClearer: leadClearer}, nil
}

// MustNewTransferConversation is the panic-on-error variant for the
// composition root.
func MustNewTransferConversation(conversations conversationReader, leadClearer conversationLeadClearer) *TransferConversation {
	u, err := NewTransferConversation(conversations, leadClearer)
	if err != nil {
		panic(err)
	}
	return u
}

// TransferConversationInput is the use-case argument.
type TransferConversationInput struct {
	TenantID       uuid.UUID
	ConversationID uuid.UUID
}

// TransferConversationResult reports the outcome. The conversation is always
// Unassigned after a successful transfer, so the result carries no payload
// today; it exists so the signature can grow without breaking callers.
type TransferConversationResult struct{}

// Execute runs the transfer pipeline: validate → load + tenant guard → clear
// the cached lead.
func (u *TransferConversation) Execute(ctx context.Context, in TransferConversationInput) (TransferConversationResult, error) {
	if in.TenantID == uuid.Nil {
		return TransferConversationResult{}, inbox.ErrInvalidTenant
	}
	if in.ConversationID == uuid.Nil {
		return TransferConversationResult{}, ErrNotFound
	}

	// Load first so the tenant guard runs against persisted truth — an
	// RLS-hidden / unknown id collapses to ErrNotFound before any write.
	if _, err := u.conversations.GetConversation(ctx, in.TenantID, in.ConversationID); err != nil {
		return TransferConversationResult{}, err
	}

	if err := u.leadClearer.ClearConversationLead(ctx, in.TenantID, in.ConversationID); err != nil {
		return TransferConversationResult{}, err
	}
	return TransferConversationResult{}, nil
}
