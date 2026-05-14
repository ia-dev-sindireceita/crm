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
type InboundEvent struct {
	TenantID          uuid.UUID
	Channel           string
	ChannelExternalID string
	SenderExternalID  string
	SenderDisplayName string
	Body              string
	OccurredAt        time.Time
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
