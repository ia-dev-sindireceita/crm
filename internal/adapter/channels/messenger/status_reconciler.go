package messenger

// Status reconciler for Messenger's two delivery-confirmation webhook
// fields, message_deliveries and message_reads. The two shapes are NOT
// symmetric:
//
//   - message_deliveries: {mids: [...], watermark} — an explicit array
//     of message ids that were delivered. Maps 1:1 onto WhatsApp's
//     per-wamid statuses[] pattern (internal/adapter/channels/whatsapp/
//     status_reconciler.go) — deliverDelivery mirrors it directly.
//   - message_reads: {watermark} — no mids array at all. Meta's
//     contract is "every message this Page sent to this user at or
//     before watermark is now read." There is no per-message read
//     event, so deliverRead resolves the conversation from the event's
//     sender PSID, lists its outbound messages, and advances every one
//     with CreatedAt <= watermark to read.
//
// Both paths funnel into the same channel-agnostic inbox.MessageStatusUpdater
// port WhatsApp already uses — Message.AdvanceStatus's monotonic
// sent→delivered→read state machine (internal/inbox/message.go) makes
// replays and out-of-order delivery/read events safe no-ops.

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
	"github.com/pericles-luz/crm/internal/inbox"
)

// envelDelivery is the message_deliveries webhook field.
type envelDelivery struct {
	MIDs      []string `json:"mids"`
	Watermark int64    `json:"watermark"` // milliseconds since epoch
}

// envelRead is the message_reads webhook field.
type envelRead struct {
	Watermark int64 `json:"watermark"` // milliseconds since epoch
}

// contactLookup is the narrow read port deliverRead needs to translate a
// message_reads event's sender PSID into a contact. *pgcontacts.Store
// satisfies it; tests inject a fake.
type contactLookup interface {
	FindByChannelIdentity(ctx context.Context, tenantID uuid.UUID, channel, externalID string) (*contacts.Contact, error)
}

// conversationLookup is the narrow read port deliverRead needs to find
// the contact's open conversation and its messages so the watermark can
// be translated into per-message inbox.StatusUpdate calls.
// *pginbox.Store satisfies it; tests inject a fake.
type conversationLookup interface {
	FindOpenConversation(ctx context.Context, tenantID, contactID uuid.UUID, channel string) (*inbox.Conversation, error)
	ListMessages(ctx context.Context, tenantID, conversationID uuid.UUID) ([]*inbox.Message, error)
}

// WithStatusUpdater wires the channel-agnostic status use case the
// message_deliveries/message_reads handlers call. Optional: when nil,
// both handlers log a warn and drop — the same fail-soft posture as
// every other optional adapter dependency.
func WithStatusUpdater(u inbox.MessageStatusUpdater) Option {
	return func(a *Adapter) {
		if u != nil {
			a.statusUpdater = u
		}
	}
}

// WithReadReceiptLookup wires the two read-only ports deliverRead needs
// to resolve a message_reads event's watermark into individual messages.
// Optional: when either is nil, deliverRead logs a warn and drops.
// message_deliveries does not need these — its mids[] array names the
// messages directly.
func WithReadReceiptLookup(c contactLookup, conv conversationLookup) Option {
	return func(a *Adapter) {
		if c != nil {
			a.contacts = c
		}
		if conv != nil {
			a.conversations = conv
		}
	}
}

// deliverDelivery processes one message_deliveries entry: one
// inbox.StatusUpdate per mid in the batch, mirroring WhatsApp's
// per-wamid deliverStatus.
func (a *Adapter) deliverDelivery(ctx context.Context, tenantID uuid.UUID, pageID string, m envelMessage) {
	if a.statusUpdater == nil {
		a.logger.Warn("messenger.status_updater_unwired",
			slog.String("tenant_id", tenantID.String()),
			slog.String("event", "message_deliveries"))
		return
	}
	occurredAt := msToTime(m.Delivery.Watermark)
	for _, rawMID := range m.Delivery.MIDs {
		mid := strings.TrimSpace(rawMID)
		if mid == "" {
			continue
		}
		a.applyStatus(ctx, tenantID, pageID, mid, inbox.MessageStatusDelivered, occurredAt)
	}
}

// deliverRead processes one message_reads entry. Meta's read receipt
// carries no per-message id — only a watermark — so this resolves the
// sender's open conversation and advances every outbound message sent
// at or before the watermark to read.
func (a *Adapter) deliverRead(ctx context.Context, tenantID uuid.UUID, pageID string, m envelMessage) {
	if a.statusUpdater == nil {
		a.logger.Warn("messenger.status_updater_unwired",
			slog.String("tenant_id", tenantID.String()),
			slog.String("event", "message_reads"))
		return
	}
	if a.contacts == nil || a.conversations == nil {
		a.logger.Warn("messenger.read_receipt_lookup_unwired",
			slog.String("tenant_id", tenantID.String()))
		return
	}
	psid := strings.TrimSpace(m.Sender.ID)
	if psid == "" {
		a.logger.Warn("messenger.read_missing_sender_psid",
			slog.String("tenant_id", tenantID.String()))
		return
	}
	watermark := msToTime(m.Read.Watermark)

	contact, err := a.contacts.FindByChannelIdentity(ctx, tenantID, Channel, psid)
	if err != nil {
		// Silent drop — anti-enumeration, same posture as an unknown
		// page id. A read receipt for a contact we never persisted an
		// inbound message from is not actionable.
		a.logger.Info("messenger.read_unknown_contact",
			slog.String("tenant_id", tenantID.String()),
			slog.String("page_id", pageID))
		return
	}
	conv, err := a.conversations.FindOpenConversation(ctx, tenantID, contact.ID, Channel)
	if err != nil {
		a.logger.Info("messenger.read_unknown_conversation",
			slog.String("tenant_id", tenantID.String()),
			slog.String("page_id", pageID))
		return
	}
	messages, err := a.conversations.ListMessages(ctx, tenantID, conv.ID)
	if err != nil {
		a.logger.Warn("messenger.read_list_messages_failed",
			slog.String("tenant_id", tenantID.String()),
			slog.String("conversation_id", conv.ID.String()),
			slog.String("err", err.Error()))
		return
	}
	for _, msg := range messages {
		if msg.Direction != inbox.MessageDirectionOut {
			continue
		}
		if msg.ChannelExternalID == "" {
			continue
		}
		if msg.CreatedAt.After(watermark) {
			continue
		}
		a.applyStatus(ctx, tenantID, pageID, msg.ChannelExternalID, inbox.MessageStatusRead, watermark)
	}
}

// applyStatus calls the status updater for one message and logs the
// outcome. Shared by deliverDelivery's per-mid loop and deliverRead's
// per-message loop.
func (a *Adapter) applyStatus(ctx context.Context, tenantID uuid.UUID, pageID, mid string, status inbox.MessageStatus, occurredAt time.Time) {
	ev := inbox.StatusUpdate{
		TenantID:          tenantID,
		Channel:           Channel,
		ChannelExternalID: mid,
		NewStatus:         status,
		OccurredAt:        occurredAt,
	}
	res, err := a.statusUpdater.HandleStatus(ctx, ev)
	if err != nil {
		a.logger.Error("messenger.status_deliver_failed",
			slog.String("tenant_id", tenantID.String()),
			slog.String("page_id", pageID),
			slog.String("mid", mid),
			slog.String("status", string(status)),
			slog.String("err", err.Error()))
		return
	}
	switch res.Outcome {
	case inbox.StatusOutcomeApplied:
		a.logger.Info("messenger.status_applied",
			slog.String("tenant_id", tenantID.String()),
			slog.String("mid", mid),
			slog.String("from", string(res.PreviousStatus)),
			slog.String("to", string(res.NewStatus)))
	case inbox.StatusOutcomeNoop:
		a.logger.Debug("messenger.status_noop",
			slog.String("tenant_id", tenantID.String()),
			slog.String("mid", mid),
			slog.String("status", string(res.NewStatus)))
	case inbox.StatusOutcomeUnknownMessage:
		a.logger.Info("messenger.status_unknown_message",
			slog.String("tenant_id", tenantID.String()),
			slog.String("mid", mid),
			slog.String("status", string(res.NewStatus)))
	}
}
