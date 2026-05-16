package inbox

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// InboundEvent is the carrier-agnostic event payload the carrier
// adapter passes into the receive-inbound use case. The webhook
// adapter (PR6) is responsible for mapping vendor payloads to this
// shape; the domain only ever sees normalized values.
//
// channel + channelExternalID is the canonical dedup key for the
// inbound_message_dedup table. SenderExternalID is the carrier-side
// identity of the sender (a phone number for WhatsApp); the use case
// resolves it to a Contact via the contacts.UpsertContactByChannel
// hook.
//
// SenderDisplayName is the fallback display name used only when a
// brand-new contact is created. It is ignored if the contact already
// exists.
//
// TenantID is the resolved tenant the event belongs to. The carrier
// adapter is expected to have done tenant resolution before calling
// the use case (typically by looking up the receiver's account in
// tenant_channel_association).
//
// HasAttachments tells the use case to materialise the message with
// `media.scan_status = "pending"` so the inbox UI never renders an
// unscanned blob to the operator (security-bar AC of F2-05). Adapters
// that surface inbound media (Messenger, Instagram) flip this on when
// the carrier payload carries attachments; the worker patches the
// row to "clean" / "infected" once a verdict lands. Text-only events
// leave it false and the persisted message has a NULL media column.
type InboundEvent struct {
	TenantID          uuid.UUID
	Channel           string
	ChannelExternalID string
	SenderExternalID  string
	SenderDisplayName string
	Body              string
	OccurredAt        time.Time
	HasAttachments    bool
}

// InboundChannel is the seam that lets the receive-inbound use case
// be invoked from a carrier-specific adapter without the use case
// importing the adapter package. The adapter implements this port
// by wiring its webhook handler to call HandleInbound on the use
// case instance held in the composition root.
//
// The port is deliberately a single method: the receive-inbound use
// case is the only entry point for an inbound message, and adding a
// second method here would create an opening for adapter-specific
// shortcuts that bypass dedup or contact resolution.
type InboundChannel interface {
	HandleInbound(ctx context.Context, ev InboundEvent) error
}

// MaterialisedInbound is the richer return shape used by adapters
// that need to correlate downstream work — notably media scans — to
// the persisted message row. Duplicate is true when the carrier
// retried an already-processed event; in that case MessageID is the
// zero UUID and callers MUST NOT republish derivative work (the
// original delivery already did so).
type MaterialisedInbound struct {
	MessageID uuid.UUID
	Duplicate bool
}

// InboundMessageMaterialiser is the richer port the Messenger /
// Instagram inbound paths bind to when they need the persisted
// MessageID for downstream fan-out (e.g. `media.scan.requested` with
// a non-nil message_id so the worker can patch `message.media`).
//
// Implementations MUST be the same code path as InboundChannel —
// dedup, contact upsert, conversation find/create, message persist,
// dedup mark — so the security and idempotency invariants do not
// fork between the two ports. *ReceiveInbound satisfies both.
type InboundMessageMaterialiser interface {
	MaterialiseInbound(ctx context.Context, ev InboundEvent) (MaterialisedInbound, error)
}
