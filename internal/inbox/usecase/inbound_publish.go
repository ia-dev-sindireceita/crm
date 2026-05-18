package usecase

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// InboundMessagePublisher is the slim outbound port that
// ReceiveInbound calls after every successful (non-duplicate) delivery
// so downstream consumers (the funnel rule engine, future AI summary
// workers) see the inbound message. Production wiring binds it to the
// JetStream publisher in internal/adapter/messaging/nats; nil disables
// the hook (the inbox keeps working with no downstream fan-out).
//
// The port matches the [engine.InboundMessage] shape verbatim so the
// adapter passes through with a single marshal. The use case stays
// engine-agnostic by holding only the wire-shaped struct here.
//
// SIN-62960 (Fase 4 funnel rule engine — NATS consumer).
type InboundMessagePublisher interface {
	PublishInboundMessage(ctx context.Context, msg PublishedInboundMessage) error
}

// PublishedInboundMessage is the slice of the persisted inbound
// message the [InboundMessagePublisher] adapter forwards onto NATS.
// Mirrors engine.InboundMessage 1:1 but lives in this package so the
// use case does not import internal/funnel/engine (the engine sits in
// the funnel bounded context — the inbox should not depend on it).
type PublishedInboundMessage struct {
	TenantID       uuid.UUID
	ConversationID uuid.UUID
	MessageID      uuid.UUID
	Channel        string
	Body           string
	OccurredAt     time.Time
}

// SetInboundMessagePublisher wires the optional outbound port. Calling
// with nil is a no-op so the wire can pass a nil through when NATS is
// disabled (NATS_URL unset, etc.). The publisher is consulted lazily
// on each Execute call so re-wiring at runtime is also safe.
//
// Matches the SetCampaignLinker pattern — using a setter rather than a
// new constructor keeps the existing NewReceiveInbound /
// NewReceiveInboundWithLeadership APIs stable.
func (u *ReceiveInbound) SetInboundMessagePublisher(p InboundMessagePublisher) {
	u.inboundPublisher = p
}

// SetInboundMessagePublisherLogger injects the logger the publisher
// hook uses for WarnContext entries. Calling with nil falls back to
// slog.Default at hook time. Production wiring passes the process
// logger; tests pass a discard logger to keep test output clean.
func (u *ReceiveInbound) SetInboundMessagePublisherLogger(l *slog.Logger) {
	u.inboundPublisherLogger = l
}

// publishInboundMessage runs the soft-fail outbound hook. It is called
// from Execute after the message has been persisted and the dedup
// ledger marked processed (so a publish failure does not lose the
// message — the inbox is the source of truth, the bus is a notification).
//
// Outcomes (all soft-fail — never abort the inbound delivery):
//
//   - publisher nil → skip silently. The wire chose not to wire the
//     NATS path (NATS_URL unset / publisher disabled).
//   - publisher returns any error → log warn and skip. The
//     funnel-engine consumer is best-effort downstream.
//
// Returns no value so Execute can ignore the call site without an
// `_ =` ceremony.
func (u *ReceiveInbound) publishInboundMessage(ctx context.Context, logger *slog.Logger, msg PublishedInboundMessage) {
	if u.inboundPublisher == nil {
		return
	}
	if err := u.inboundPublisher.PublishInboundMessage(ctx, msg); err != nil {
		logger.WarnContext(ctx, "inbox: inbound publish failed",
			slog.String("tenant_id", msg.TenantID.String()),
			slog.String("conversation_id", msg.ConversationID.String()),
			slog.String("message_id", msg.MessageID.String()),
			slog.String("err", err.Error()),
		)
	}
}
