package nats

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/funnel/engine"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// inboundMessagePublishTarget is the JetStream surface the
// InboundMessagePublisher writes to. The SDKAdapter satisfies it
// directly via PublishMsgID; tests inject a recording fake.
type inboundMessagePublishTarget interface {
	PublishMsgID(ctx context.Context, subject, msgID string, body []byte) error
}

// InboundMessagePublisher publishes the inbox's inbound-message
// envelope onto the funnel engine's JetStream subject ([engine.Subject]).
// The publisher is wired from inbox.ReceiveInbound after MarkProcessed
// so the rule engine sees every persisted inbound message exactly
// once.
//
// Subject + StreamName are imported from the consumer package
// (internal/funnel/engine) so the producer cannot drift from the
// contract the worker pins.
//
// Dedup: the Nats-Msg-Id header is set to the message uuid (the same
// id the engine uses as its idempotency key). Within the JetStream
// Duplicates window (1h, enforced by EnsureStream) a re-publish
// collapses to a single delivery. This complements the consumer's
// (rule_id, message_id) UNIQUE constraint, which catches duplicates
// that arrive after the window expires.
//
// The publisher is intentionally soft-fail at the inbox call site
// (inbox.ReceiveInbound logs and ignores publish errors so the
// inbound message remains persisted even if NATS is down). The
// adapter itself returns wrapped errors so the caller decides —
// adapters do not silently swallow.
type InboundMessagePublisher struct {
	target inboundMessagePublishTarget
}

// NewInboundMessagePublisher wraps target. target is the
// already-connected JetStream surface (an *SDKAdapter in production);
// nil is rejected so a misconfigured boot fails fast.
func NewInboundMessagePublisher(target inboundMessagePublishTarget) (*InboundMessagePublisher, error) {
	if target == nil {
		return nil, errors.New("nats: InboundMessagePublisher target is required")
	}
	return &InboundMessagePublisher{target: target}, nil
}

// PublishInboundMessage encodes the inbox-side message into the engine
// wire envelope and publishes it on [engine.Subject] with Nats-Msg-Id
// set to the message uuid. Validates required fields; an invalid input
// yields [engine.ErrInvalidEvent] wrapped so the caller can match on
// errors.Is.
//
// Compile-time fence keeps the adapter in sync with the inbox port —
// see the var _ at the bottom of the file.
func (p *InboundMessagePublisher) PublishInboundMessage(ctx context.Context, msg inboxusecase.PublishedInboundMessage) error {
	if msg.MessageID == uuid.Nil {
		return fmt.Errorf("nats: %w: nil message id", engine.ErrInvalidEvent)
	}
	body, err := engine.EncodeInboundMessage(engine.InboundMessage{
		TenantID:       msg.TenantID,
		ConversationID: msg.ConversationID,
		MessageID:      msg.MessageID,
		Channel:        msg.Channel,
		Body:           msg.Body,
		OccurredAt:     msg.OccurredAt,
	})
	if err != nil {
		return fmt.Errorf("nats: encode inbound message: %w", err)
	}
	if err := p.target.PublishMsgID(ctx, engine.Subject, msg.MessageID.String(), body); err != nil {
		return fmt.Errorf("nats: publish inbound message: %w", err)
	}
	return nil
}

// Compile-time fence — keep the port and adapter in sync at build time.
var _ inboxusecase.InboundMessagePublisher = (*InboundMessagePublisher)(nil)
