// Package usecase holds the application services for the inbox
// aggregate. PR4 ships ReceiveInbound (the carrier-adapter entry
// point) and SendOutbound (the HTMX-handler entry point). The full
// dependency map:
//
//	ReceiveInbound  ← InboundChannel adapter (PR6/7/8)
//	  ├─ InboundDedupRepository.Claim/MarkProcessed       (this PR)
//	  ├─ contacts.UpsertContactByChannel                  (PR3)
//	  └─ Repository.FindOpenConversation/CreateConversation/SaveMessage (this PR)
//
//	SendOutbound    ← HTMX outbound handler (PR7+)
//	  ├─ Repository.GetConversation/SaveMessage/UpdateMessage (this PR)
//	  ├─ WalletDebitor.Debit                              (PR5 implementation)
//	  └─ OutboundChannel.SendMessage                      (PR8)
//
// Use-cases depend on ports; they never import database drivers or
// vendor SDKs. Wiring (which implementation is plugged in) is done at
// the composition root in cmd/server.
package usecase

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	"github.com/pericles-luz/crm/internal/inbox"
)

// ContactUpserter is the slim subset of contacts/usecase the
// receive-inbound flow needs. Decoupling on the method signature lets
// tests inject a fake contact resolver without spinning up Postgres.
// The production wiring binds it to *contactsusecase.UpsertContactByChannel.
type ContactUpserter interface {
	Execute(ctx context.Context, in contactsusecase.Input) (contactsusecase.Result, error)
}

// ReceiveInbound orchestrates a webhook-style inbound delivery:
//
//  1. Claim the (channel, channel_external_id) pair on the global dedup
//     ledger. If already claimed → return nil (the carrier is retrying;
//     we MUST NOT double-emit a message).
//  2. Resolve / create the Contact via UpsertContactByChannel.
//  3. Find an open Conversation for (tenant, contact, channel) or
//     create one.
//  4. Save the Message + bump LastMessageAt on the conversation.
//  5. MarkProcessed on the dedup ledger.
//
// Idempotency contract: calling Execute twice for the same
// (channel, channel_external_id) MUST result in exactly one persisted
// message and exactly one persisted contact (AC #4).
type ReceiveInbound struct {
	repo     inbox.Repository
	dedup    inbox.InboundDedupRepository
	contacts ContactUpserter
}

// NewReceiveInbound wires the use case to its dependencies. nil port
// arguments are programming errors caught at construction so the
// process crashes before serving the first request.
func NewReceiveInbound(repo inbox.Repository, dedup inbox.InboundDedupRepository, c ContactUpserter) (*ReceiveInbound, error) {
	if repo == nil {
		return nil, errors.New("inbox/usecase: repo must not be nil")
	}
	if dedup == nil {
		return nil, errors.New("inbox/usecase: dedup must not be nil")
	}
	if c == nil {
		return nil, errors.New("inbox/usecase: contacts upserter must not be nil")
	}
	return &ReceiveInbound{repo: repo, dedup: dedup, contacts: c}, nil
}

// MustNewReceiveInbound is the panic-on-error variant for the
// composition root.
func MustNewReceiveInbound(repo inbox.Repository, dedup inbox.InboundDedupRepository, c ContactUpserter) *ReceiveInbound {
	u, err := NewReceiveInbound(repo, dedup, c)
	if err != nil {
		panic(err)
	}
	return u
}

// ReceiveInboundResult reports the outcome of an inbound delivery. The
// boolean Duplicate is true when dedup rejected the event — useful for
// metrics and for the carrier adapter to choose the right HTTP ack.
type ReceiveInboundResult struct {
	Conversation *inbox.Conversation
	Message      *inbox.Message
	Contact      *contacts.Contact
	Duplicate    bool
}

// Execute runs the inbound pipeline. See type-level doc-comment for
// the full algorithm.
func (u *ReceiveInbound) Execute(ctx context.Context, ev inbox.InboundEvent) (ReceiveInboundResult, error) {
	if ev.TenantID == uuid.Nil {
		return ReceiveInboundResult{}, inbox.ErrInvalidTenant
	}
	channel := strings.ToLower(strings.TrimSpace(ev.Channel))
	if channel == "" {
		return ReceiveInboundResult{}, inbox.ErrInvalidChannel
	}
	externalID := strings.TrimSpace(ev.ChannelExternalID)
	if externalID == "" {
		return ReceiveInboundResult{}, inbox.ErrInvalidStatus
	}
	if strings.TrimSpace(ev.SenderExternalID) == "" {
		return ReceiveInboundResult{}, inbox.ErrInvalidChannel
	}

	// 1. Dedup claim.
	if err := u.dedup.Claim(ctx, channel, externalID); err != nil {
		if errors.Is(err, inbox.ErrInboundAlreadyProcessed) {
			return ReceiveInboundResult{Duplicate: true}, nil
		}
		return ReceiveInboundResult{}, err
	}

	// 2. Resolve / create the contact.
	res, err := u.contacts.Execute(ctx, contactsusecase.Input{
		TenantID:    ev.TenantID,
		Channel:     channel,
		ExternalID:  ev.SenderExternalID,
		DisplayName: fallbackDisplay(ev.SenderDisplayName, ev.SenderExternalID),
	})
	if err != nil {
		return ReceiveInboundResult{}, err
	}

	// 3. Find an open conversation or create one.
	conv, err := u.repo.FindOpenConversation(ctx, ev.TenantID, res.Contact.ID, channel)
	if err != nil && !errors.Is(err, inbox.ErrNotFound) {
		return ReceiveInboundResult{}, err
	}
	if errors.Is(err, inbox.ErrNotFound) {
		conv, err = inbox.NewConversation(ev.TenantID, res.Contact.ID, channel)
		if err != nil {
			return ReceiveInboundResult{}, err
		}
		if err := u.repo.CreateConversation(ctx, conv); err != nil {
			return ReceiveInboundResult{}, err
		}
	}

	// 4. Persist the message and bump LastMessageAt.
	m, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID:          ev.TenantID,
		ConversationID:    conv.ID,
		Direction:         inbox.MessageDirectionIn,
		Body:              ev.Body,
		ChannelExternalID: externalID,
	})
	if err != nil {
		return ReceiveInboundResult{}, err
	}
	if !ev.OccurredAt.IsZero() {
		m.CreatedAt = ev.OccurredAt
	}
	if err := conv.RecordMessage(m); err != nil {
		return ReceiveInboundResult{}, err
	}
	if err := u.repo.SaveMessage(ctx, m); err != nil {
		return ReceiveInboundResult{}, err
	}

	// 5. Close the dedup row.
	if err := u.dedup.MarkProcessed(ctx, channel, externalID); err != nil {
		return ReceiveInboundResult{}, err
	}

	return ReceiveInboundResult{
		Conversation: conv,
		Message:      m,
		Contact:      res.Contact,
		Duplicate:    false,
	}, nil
}

// fallbackDisplay picks a display name when the carrier did not send
// a profile name. Execute already rejects events with a blank
// SenderExternalID, so sender is always non-empty when this is called
// — the phone number is a reasonable last resort.
func fallbackDisplay(profile, sender string) string {
	if s := strings.TrimSpace(profile); s != "" {
		return s
	}
	return strings.TrimSpace(sender)
}
