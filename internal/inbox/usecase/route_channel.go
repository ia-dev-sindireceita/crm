package usecase

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// SetChannelResolver wires the optional SIN-66378 P4 routing port.
// Calling with nil is a no-op so the wire can disable routing (the
// conversation is created with a NULL channel_id, the pre-P4 behaviour).
// The resolver is consulted lazily on each Execute call so re-wiring at
// runtime is also safe.
//
// Matches the SetCampaignLinker / SetInboundMessagePublisher pattern —
// a setter rather than a new constructor keeps the existing
// NewReceiveInbound / NewReceiveInboundWithLeadership APIs stable.
func (u *ReceiveInbound) SetChannelResolver(r ChannelResolver) {
	u.channelResolver = r
}

// SetChannelResolverLogger injects the logger the routing hook uses for
// WarnContext entries. Calling with nil falls back to slog.Default at
// hook time. Production wiring passes the process logger; tests pass a
// discard logger to keep test output clean.
func (u *ReceiveInbound) SetChannelResolverLogger(l *slog.Logger) {
	u.channelResolverLogger = l
}

// routeConversation binds a freshly-created conversation to the tenant
// channel instance resolved from the inbound identity, so
// conversation.channel_id references the instance rather than the bare
// carrier string (SIN-66378 P4). It runs before the first persist and is
// soft-fail — routing is best-effort and must never drop an inbound
// message:
//
//   - resolver nil → skip silently (routing disabled: the conversation
//     keeps a NULL channel_id, the pre-P4 behaviour);
//   - resolver returns an error → log warn and skip (the message is still
//     persisted; the conversation stays unrouted rather than lost);
//   - resolver returns uuid.Nil → no instance matched; leave channel_id
//     NULL (RouteToChannel is a no-op on uuid.Nil).
//
// channelKey is the normalised carrier; destinationExternalID is the
// tenant-side address the message arrived on (empty when the carrier does
// not surface it — the resolver then falls back to the tenant's instance
// for the carrier).
func (u *ReceiveInbound) routeConversation(ctx context.Context, conv *inbox.Conversation, channelKey, destinationExternalID string) {
	if u.channelResolver == nil {
		return
	}
	channelID, err := u.channelResolver.ResolveChannelID(ctx, conv.TenantID, channelKey, destinationExternalID)
	if err != nil {
		u.channelResolverLog().WarnContext(ctx, "inbox: channel routing failed; conversation left unrouted",
			slog.String("tenant_id", conv.TenantID.String()),
			slog.String("conversation_id", conv.ID.String()),
			slog.String("channel", channelKey),
			slog.Any("error", err),
		)
		return
	}
	if channelID == uuid.Nil {
		return
	}
	conv.RouteToChannel(channelID)
}

// channelResolverLog returns the configured routing logger or slog.Default.
func (u *ReceiveInbound) channelResolverLog() *slog.Logger {
	if u.channelResolverLogger != nil {
		return u.channelResolverLogger
	}
	return slog.Default()
}
