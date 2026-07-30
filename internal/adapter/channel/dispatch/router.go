package dispatch

import (
	"context"
	"fmt"

	"github.com/pericles-luz/crm/internal/inbox"
)

// Router dispatches an OutboundMessage to the carrier adapter registered
// for its Channel. It implements inbox.OutboundChannel so the
// send-outbound use case stays carrier-agnostic: the use case sets
// OutboundMessage.Channel from the conversation and the Router picks the
// concrete Sender.
//
// Deny-by-default: a message whose Channel has no registered route (or a
// nil route) resolves to inbox.ErrChannelDisabled without any carrier
// call. An empty Router is therefore a graceful no-op — the shape the
// composition root wires when a carrier's outbound credentials are
// absent, so boot never fails and no outbound HTTP is issued.
type Router struct {
	routes map[string]inbox.OutboundChannel
}

// NewRouter builds a Router from a channel→adapter map. The map is copied
// defensively (nil entries are dropped) so later mutation of the caller's
// map cannot re-point a live route. A nil or empty map yields a Router
// that disables every channel — the deny-by-default no-op.
func NewRouter(routes map[string]inbox.OutboundChannel) *Router {
	copied := make(map[string]inbox.OutboundChannel, len(routes))
	for ch, oc := range routes {
		if ch == "" || oc == nil {
			continue
		}
		copied[ch] = oc
	}
	return &Router{routes: copied}
}

// SendMessage implements inbox.OutboundChannel. It looks up the adapter
// for m.Channel and delegates; an unrouted channel returns
// inbox.ErrChannelDisabled (wrapped with the channel name for the
// operator-facing error) and issues no carrier request.
func (r *Router) SendMessage(ctx context.Context, m inbox.OutboundMessage) (string, error) {
	oc, ok := r.routes[m.Channel]
	if !ok || oc == nil {
		return "", fmt.Errorf("%w: no outbound route for channel %q", inbox.ErrChannelDisabled, m.Channel)
	}
	return oc.SendMessage(ctx, m)
}

// Compile-time assertion that Router implements inbox.OutboundChannel.
var _ inbox.OutboundChannel = (*Router)(nil)
