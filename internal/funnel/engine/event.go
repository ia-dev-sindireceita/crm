package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Subject is the JetStream subject the engine consumer subscribes to.
// Producers — currently only the inbox.ReceiveInbound hook wired in
// cmd/server — MUST publish here for every persisted inbound message.
// The string is exported (consumer + producer share it) so the wire
// contract is enforced at compile time.
const Subject = "inbox.messages.received"

// StreamName is the JetStream stream the worker expects to read from.
// EnsureStream lives on the worker entrypoint (mirrors wallet_alerter /
// mediascan-worker — the stream definition is owned by the consumer).
const StreamName = "INBOX"

// QueueName is the JetStream queue group. Multiple replicas share the
// group so each message is handed to exactly one replica.
const QueueName = "funnel-engine"

// DurableName is the JetStream durable consumer name. Reuse across
// process restarts so the cursor survives a crash. The `-v1` suffix
// reserves naming room for a non-backwards-compatible payload change.
const DurableName = "funnel-engine-v1"

// InboundMessageEvent is the JSON envelope carried on [Subject]. The
// shape is intentionally narrow: only the fields the engine needs to
// resolve rules and dispatch the action are on the wire, so a future
// payload addition (e.g. team_id, contact_id) can ship without bumping
// the consumer that ignores them.
//
// All time.Time fields are RFC3339Nano UTC strings; the consumer
// normalises them with UTC() before logging so timezones do not leak.
type InboundMessageEvent struct {
	TenantID       string    `json:"tenant_id"`
	ConversationID string    `json:"conversation_id"`
	MessageID      string    `json:"message_id"`
	Channel        string    `json:"channel"`
	Body           string    `json:"body"`
	OccurredAt     time.Time `json:"occurred_at"`
}

// EncodeInboundMessage marshals an [InboundMessage] domain value into
// the wire envelope a publisher writes to NATS. The function lives in
// the engine package so producer and consumer share the encoder; a
// future schema change (versioning, base64 envelope) lands here once.
func EncodeInboundMessage(m InboundMessage) ([]byte, error) {
	wire := InboundMessageEvent{
		TenantID:       m.TenantID.String(),
		ConversationID: m.ConversationID.String(),
		MessageID:      m.MessageID.String(),
		Channel:        m.Channel,
		Body:           m.Body,
		OccurredAt:     m.OccurredAt.UTC(),
	}
	return json.Marshal(wire)
}

// DecodeInboundMessage parses the wire envelope and validates the
// required fields. A poison payload returns a wrapped [ErrInvalidEvent]
// the caller (worker handler) turns into an Ack-and-log so JetStream
// stops redelivering it.
func DecodeInboundMessage(raw []byte) (InboundMessage, error) {
	if len(raw) == 0 {
		return InboundMessage{}, ErrInvalidEvent
	}
	var ev InboundMessageEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return InboundMessage{}, errors.Join(ErrInvalidEvent, err)
	}
	tenantID, err := uuid.Parse(strings.TrimSpace(ev.TenantID))
	if err != nil || tenantID == uuid.Nil {
		return InboundMessage{}, ErrInvalidEvent
	}
	conversationID, err := uuid.Parse(strings.TrimSpace(ev.ConversationID))
	if err != nil || conversationID == uuid.Nil {
		return InboundMessage{}, ErrInvalidEvent
	}
	messageID, err := uuid.Parse(strings.TrimSpace(ev.MessageID))
	if err != nil || messageID == uuid.Nil {
		return InboundMessage{}, ErrInvalidEvent
	}
	channel := strings.TrimSpace(ev.Channel)
	if channel == "" {
		return InboundMessage{}, ErrInvalidEvent
	}
	occurredAt := ev.OccurredAt
	if occurredAt.IsZero() {
		return InboundMessage{}, ErrInvalidEvent
	}
	return InboundMessage{
		TenantID:       tenantID,
		ConversationID: conversationID,
		MessageID:      messageID,
		Channel:        strings.ToLower(channel),
		Body:           ev.Body,
		OccurredAt:     occurredAt.UTC(),
	}, nil
}
