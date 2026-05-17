// Package aiassistinvalidator hosts the AISummary cache invalidator
// worker for SIN-62908 / Fase 3 W4D. The worker subscribes to the
// `message.created` JetStream subject and tells the aiassist use case
// to invalidate the cached summary for the affected (tenant_id,
// conversation_id) pair so the next operator-triggered Summarize misses
// the cache and regenerates against the new conversation tail.
//
// Idempotency rules:
//
//   - Invalidate is idempotent at the domain level (Summary.Invalidate
//     is a no-op on already-invalidated rows), so a JetStream
//     redelivery never causes a duplicate side-effect.
//   - We still pass through the Nats-Msg-Id on logs so an operator can
//     correlate publisher and consumer when triaging cache anomalies.
//
// Domain dependencies are kept minimal: the only collaborator is the
// Invalidator port (which *aiassistusecase.Service satisfies). The
// JetStream binding lives behind EventSubscriber + Delivery — same
// shapes as wallet's worker (SIN-62881) so the cmd/server wireup can
// adopt the existing pattern without surprises.
package aiassistinvalidator

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// SubjectMessageCreated is the JetStream subject this worker binds to.
// The inbound and outbound message pipelines (internal/inbox/usecase
// receive + send) are expected to publish a `message.created` event
// whenever a new row lands on inbox.message; this worker drains those
// events and invalidates the matching AISummary. The constant is
// defined here so the publisher and subscriber agree on the wire name
// without forcing a cross-package import.
const SubjectMessageCreated = "message.created"

// Event is the JSON payload decoded from each message.created
// delivery. The shape is the smallest pair the invalidator needs;
// callers (publishers) may include additional metadata fields without
// breaking compatibility — encoding/json ignores unknown fields.
type Event struct {
	TenantID       uuid.UUID `json:"tenant_id"`
	ConversationID uuid.UUID `json:"conversation_id"`
	// MessageID is informational only — the invalidator does not key
	// off it. Included so log traces can map back to the originating
	// row.
	MessageID uuid.UUID `json:"message_id,omitempty"`
	// CreatedAt is informational only.
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// Delivery is the small surface the worker needs from each JetStream
// message: the raw body to decode, the Nats-Msg-Id header (for log
// correlation), and the per-delivery Ack/Nak controls.
//
// Subscriber-side hygiene rules:
//
//   - Ack is called exactly once on success; double-Ack is a no-op.
//   - Nak signals JetStream to redeliver after delay. The broker still
//     respects MaxDeliver for the durable consumer; once exhausted the
//     adapter is expected to emit a DLQ message and Ack the original.
type Delivery interface {
	Data() []byte
	MsgID() string
	Ack(ctx context.Context) error
	Nak(ctx context.Context, delay time.Duration) error
}

// EventSubscriber is the port the worker uses to consume from
// JetStream. Subscribe returns a channel of deliveries plus a terminal
// error channel; cancelling ctx drains the subscription.
type EventSubscriber interface {
	Subscribe(ctx context.Context) (<-chan Delivery, <-chan error, error)
}

// Invalidator is the application port the worker drives. The aiassist
// use case's *Service.Invalidate satisfies it structurally.
type Invalidator interface {
	Invalidate(ctx context.Context, tenantID, conversationID uuid.UUID) error
}
