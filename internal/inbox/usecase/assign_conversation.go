package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// AssignConversation is the write-side use case that records a manual
// leadership decision on a conversation (SIN-64978): an operator assigns
// (or re-assigns) a conversation to an attendant/gerente of the same
// tenant. It owns three domain invariants the web layer MUST NOT be
// trusted to enforce:
//
//  1. Role + tenant gate — the target user MUST be an assignable attendant
//     under the same tenant (deny-by-default; a cross-tenant or non-inbox
//     user is rejected with ErrUserNotAssignable). This guards the
//     targetUserID IDOR.
//  2. Conversation isolation — the conversation MUST exist under the
//     tenant scope; an RLS-hidden id collapses to ErrNotFound. This guards
//     the conversationID IDOR.
//  3. Append-only audit — every assignment writes one assignment_history
//     row via the ledger port; re-assigning to the current lead is a typed
//     no-op (ErrAlreadyAssigned) rather than a duplicate row.
//
// The reason is derived, never supplied by the caller: the first
// attribution is LeadReasonManual, a hand-off while a lead already exists
// is LeadReasonReassign. The auto-attribution path (LeadReasonLead) lives
// in ReceiveInbound, not here.
//
// No SQL or transport lives here — the use case talks only to ports.
type AssignConversation struct {
	conversations conversationReader
	ledger        leadLedger
	leadCache     inbox.ConversationLeadStore
	attendants    attendantGate
}

// The conversation read seam (GetConversation) is the package-shared
// conversationReader interface declared in get_conversation_context.go —
// satisfied by inbox.Repository (the postgres inbox Store). It gives the
// tenant-isolation guard and the closed-conversation invariant.

// leadLedger is the append-only assignment_history seam: read the current
// lead (to derive reason + detect the no-op) and append the new row.
type leadLedger interface {
	LatestAssignment(ctx context.Context, tenantID, conversationID uuid.UUID) (*inbox.Assignment, error)
	AppendHistory(ctx context.Context, tenantID, conversationID, userID uuid.UUID, reason inbox.LeadReason) (*inbox.Assignment, error)
}

// attendantGate is the narrow point-check the use case needs from the
// attendant directory: is this user assignable under this tenant?
type attendantGate interface {
	IsAssignable(ctx context.Context, tenantID, userID uuid.UUID) (bool, error)
}

// AssignConversationInput is the use-case argument. All three ids are
// required; the reason is derived, not supplied.
type AssignConversationInput struct {
	TenantID       uuid.UUID
	ConversationID uuid.UUID
	// TargetUserID is the attendant/gerente to lead the conversation. It is
	// validated against the tenant role gate — the caller MUST NOT be
	// trusted to have checked tenancy or role.
	TargetUserID uuid.UUID
}

// AssignConversationResult carries the persisted assignment_history row so
// the handler can render the new lead (id, assigned_at, reason) without a
// second round-trip.
type AssignConversationResult struct {
	Assignment *inbox.Assignment
}

// NewAssignConversation wires the use case. All four ports are required
// except none may be nil; leadCache keeps the read-model's denormalised
// lead column coherent with the ledger.
func NewAssignConversation(
	conversations conversationReader,
	ledger leadLedger,
	leadCache inbox.ConversationLeadStore,
	attendants attendantGate,
) (*AssignConversation, error) {
	if conversations == nil {
		return nil, errors.New("inbox/usecase: conversation reader must not be nil")
	}
	if ledger == nil {
		return nil, errors.New("inbox/usecase: lead ledger must not be nil")
	}
	if leadCache == nil {
		return nil, errors.New("inbox/usecase: lead cache must not be nil")
	}
	if attendants == nil {
		return nil, errors.New("inbox/usecase: attendant gate must not be nil")
	}
	return &AssignConversation{
		conversations: conversations,
		ledger:        ledger,
		leadCache:     leadCache,
		attendants:    attendants,
	}, nil
}

// MustNewAssignConversation is the panic-on-error variant for the
// composition root.
func MustNewAssignConversation(
	conversations conversationReader,
	ledger leadLedger,
	leadCache inbox.ConversationLeadStore,
	attendants attendantGate,
) *AssignConversation {
	u, err := NewAssignConversation(conversations, ledger, leadCache, attendants)
	if err != nil {
		panic(err)
	}
	return u
}

// Execute runs the assignment pipeline: validate → role/tenant gate →
// load conversation → derive reason / detect no-op → append history →
// sync the denormalised lead cache.
func (u *AssignConversation) Execute(ctx context.Context, in AssignConversationInput) (AssignConversationResult, error) {
	if in.TenantID == uuid.Nil {
		return AssignConversationResult{}, inbox.ErrInvalidTenant
	}
	if in.ConversationID == uuid.Nil {
		return AssignConversationResult{}, inbox.ErrNotFound
	}
	if in.TargetUserID == uuid.Nil {
		return AssignConversationResult{}, inbox.ErrInvalidAssignee
	}

	// 1. Role + tenant gate on the target user (deny-by-default). Runs
	//    before the conversation load so a probe with a cross-tenant /
	//    non-inbox target never reveals whether the conversation exists.
	ok, err := u.attendants.IsAssignable(ctx, in.TenantID, in.TargetUserID)
	if err != nil {
		return AssignConversationResult{}, err
	}
	if !ok {
		return AssignConversationResult{}, inbox.ErrUserNotAssignable
	}

	// 2. Load the conversation under the tenant scope — RLS-hidden ids
	//    collapse to ErrNotFound (conversationID IDOR guard).
	conv, err := u.conversations.GetConversation(ctx, in.TenantID, in.ConversationID)
	if err != nil {
		return AssignConversationResult{}, err
	}

	// 3. Derive the reason from the current lead and detect the no-op.
	latest, err := u.ledger.LatestAssignment(ctx, in.TenantID, in.ConversationID)
	hasLead := err == nil
	if err != nil && !errors.Is(err, inbox.ErrNotFound) {
		return AssignConversationResult{}, err
	}
	reason := inbox.LeadReasonManual
	if hasLead {
		if latest.UserID == in.TargetUserID {
			return AssignConversationResult{}, inbox.ErrAlreadyAssigned
		}
		reason = inbox.LeadReasonReassign
	}

	// 4. Apply the domain transition. AssignTo enforces the
	//    closed-conversation invariant (ErrConversationClosed) and the
	//    assignee/reason invariants; the in-memory Assignment it returns is
	//    superseded by the persisted row below.
	if _, err := conv.AssignTo(in.TargetUserID, reason); err != nil {
		return AssignConversationResult{}, err
	}

	// 5. Persist the audit row, then sync the read-model's cached lead.
	saved, err := u.ledger.AppendHistory(ctx, in.TenantID, in.ConversationID, in.TargetUserID, reason)
	if err != nil {
		return AssignConversationResult{}, err
	}
	if err := u.leadCache.SetConversationLead(ctx, in.TenantID, in.ConversationID, in.TargetUserID); err != nil {
		return AssignConversationResult{}, err
	}

	return AssignConversationResult{Assignment: saved}, nil
}
