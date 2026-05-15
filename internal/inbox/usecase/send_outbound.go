package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// SendOutbound orchestrates an outbound delivery from an operator
// composing a reply through the HTMX inbox UI:
//
//  1. Load the Conversation (tenant-scoped).
//  2. Build the pending outbound Message.
//  3. WalletDebitor.Debit reserves cost, runs charge (which contains
//     SaveMessage(pending) + OutboundChannel.SendMessage + UpdateMessage
//     with sent/wamid), and commits on success / refunds on error.
//
// AC #5 requires that the wallet is exercised even when cost == 0;
// the implementation MUST still call charge so the bookkeeping path
// runs uniformly. The contract is documented on WalletDebitor.Debit.
type SendOutbound struct {
	repo      inbox.Repository
	wallet    inbox.WalletDebitor
	outbound  inbox.OutboundChannel
	costFn    func(ctx context.Context, m inbox.OutboundMessage) (int64, error)
	contactID func(ctx context.Context, tenantID, conversationID uuid.UUID) (string, error)
}

// SendOutboundInput is the use-case argument. ToExternalID is the
// carrier identity of the recipient. When zero, the use case looks it
// up via the contactLookup hook supplied to NewSendOutbound — wired
// in PR8 to the contacts adapter. For PR4 tests, callers supply the
// recipient explicitly.
type SendOutboundInput struct {
	TenantID       uuid.UUID
	ConversationID uuid.UUID
	Body           string
	SentByUserID   *uuid.UUID
	ToExternalID   string
}

// SendOutboundResult reports the outcome of an outbound send.
type SendOutboundResult struct {
	Message *inbox.Message
}

// CostFn returns the per-message token cost. The default implementation
// returns 0 (every send is free); production wiring in PR9 will swap
// in a tariff-table-backed function.
type CostFn func(ctx context.Context, m inbox.OutboundMessage) (int64, error)

// ContactLookupFn returns the carrier external-id of the contact
// associated with a conversation. PR8 wires this through the
// contacts adapter; PR4 tests inject a stub.
type ContactLookupFn func(ctx context.Context, tenantID, conversationID uuid.UUID) (string, error)

// SendOutboundOption configures optional dependencies. CostFn defaults
// to "free" (returns 0). ContactLookupFn defaults to "use the
// To-ExternalID supplied on the input".
type SendOutboundOption func(*SendOutbound)

// WithCost configures a non-zero cost function. The cost may legitimately
// be zero — the wallet adapter still commits a zero-amount reservation
// so the bookkeeping path is exercised uniformly (AC #5).
func WithCost(fn CostFn) SendOutboundOption {
	return func(u *SendOutbound) { u.costFn = fn }
}

// WithContactLookup configures how the use case resolves a
// conversation to a carrier recipient id. Useful when the input does
// not carry ToExternalID directly.
func WithContactLookup(fn ContactLookupFn) SendOutboundOption {
	return func(u *SendOutbound) { u.contactID = fn }
}

// NewSendOutbound wires the use case. nil port arguments are caught
// at construction.
func NewSendOutbound(repo inbox.Repository, wallet inbox.WalletDebitor, outbound inbox.OutboundChannel, opts ...SendOutboundOption) (*SendOutbound, error) {
	if repo == nil {
		return nil, errors.New("inbox/usecase: repo must not be nil")
	}
	if wallet == nil {
		return nil, errors.New("inbox/usecase: wallet must not be nil")
	}
	if outbound == nil {
		return nil, errors.New("inbox/usecase: outbound channel must not be nil")
	}
	u := &SendOutbound{
		repo:     repo,
		wallet:   wallet,
		outbound: outbound,
		costFn:   func(ctx context.Context, m inbox.OutboundMessage) (int64, error) { return 0, nil },
	}
	for _, opt := range opts {
		opt(u)
	}
	return u, nil
}

// MustNewSendOutbound is the panic-on-error variant for the composition root.
func MustNewSendOutbound(repo inbox.Repository, wallet inbox.WalletDebitor, outbound inbox.OutboundChannel, opts ...SendOutboundOption) *SendOutbound {
	u, err := NewSendOutbound(repo, wallet, outbound, opts...)
	if err != nil {
		panic(err)
	}
	return u
}

// Execute runs the outbound pipeline.
func (u *SendOutbound) Execute(ctx context.Context, in SendOutboundInput) (SendOutboundResult, error) {
	if in.TenantID == uuid.Nil {
		return SendOutboundResult{}, inbox.ErrInvalidTenant
	}
	if in.ConversationID == uuid.Nil {
		return SendOutboundResult{}, inbox.ErrInvalidContact
	}
	conv, err := u.repo.GetConversation(ctx, in.TenantID, in.ConversationID)
	if err != nil {
		return SendOutboundResult{}, err
	}
	if conv.State != inbox.ConversationStateOpen {
		return SendOutboundResult{}, inbox.ErrConversationClosed
	}

	to := in.ToExternalID
	if to == "" && u.contactID != nil {
		to, err = u.contactID(ctx, in.TenantID, in.ConversationID)
		if err != nil {
			return SendOutboundResult{}, err
		}
	}

	out := inbox.OutboundMessage{
		TenantID:       in.TenantID,
		ConversationID: in.ConversationID,
		Channel:        conv.Channel,
		ToExternalID:   to,
		Body:           in.Body,
	}

	cost, err := u.costFn(ctx, out)
	if err != nil {
		return SendOutboundResult{}, err
	}

	m, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID:       in.TenantID,
		ConversationID: in.ConversationID,
		Direction:      inbox.MessageDirectionOut,
		Body:           in.Body,
		SentByUserID:   in.SentByUserID,
	})
	if err != nil {
		return SendOutboundResult{}, err
	}

	debitErr := u.wallet.Debit(ctx, in.TenantID, cost, func(ctx context.Context) error {
		// Persist the pending row first so a crash mid-charge leaves a
		// "pending" trail the operator can investigate.
		if err := u.repo.SaveMessage(ctx, m); err != nil {
			return err
		}
		channelExternalID, sendErr := u.outbound.SendMessage(ctx, out)
		if sendErr != nil {
			// Best-effort failure mark; if UpdateMessage also fails the
			// message will be left pending and reconciler logic from
			// SIN-62727 (F37) will close the loop later.
			_ = m.AdvanceStatus(inbox.MessageStatusFailed)
			_ = u.repo.UpdateMessage(ctx, m)
			return sendErr
		}
		if channelExternalID != "" {
			if err := m.AttachChannelExternalID(channelExternalID); err != nil {
				return err
			}
		}
		if err := m.AdvanceStatus(inbox.MessageStatusSent); err != nil {
			return err
		}
		if err := u.repo.UpdateMessage(ctx, m); err != nil {
			return err
		}
		return nil
	})
	if debitErr != nil {
		return SendOutboundResult{}, debitErr
	}

	return SendOutboundResult{Message: m}, nil
}

// SendForView is the web-facing wrapper that runs Execute and projects
// the resulting domain Message onto a MessageView. The HTMX inbox
// handler (internal/web/inbox) calls this variant so it does not import
// the inbox domain root — keeping forbidwebboundary happy.
func (u *SendOutbound) SendForView(ctx context.Context, in SendOutboundInput) (MessageView, error) {
	res, err := u.Execute(ctx, in)
	if err != nil {
		return MessageView{}, err
	}
	return messageToView(res.Message), nil
}
