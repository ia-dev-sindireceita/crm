package dispatch

import (
	"context"
	"fmt"
	"time"

	"github.com/pericles-luz/crm/internal/inbox"
)

// RateLimiter is the per-key counter port the outbound path shares with
// the inbound WhatsApp adapter (internal/adapter/channels/whatsapp.RateLimiter
// has the same shape). Defining it locally — accept-broad — lets the
// redis sliding-window adapter plug in at the composition root without
// this package importing the carrier package or the redis client.
type RateLimiter interface {
	Allow(ctx context.Context, key string, window time.Duration, max int) (allowed bool, retryAfter time.Duration, err error)
}

// KeyFunc derives the rate-limit bucket key for an OutboundMessage. The
// default (DefaultKeyFunc) buckets per tenant so one tenant's outbound
// burst cannot starve neighbours; callers can override to bucket per
// conversation or per carrier identity.
type KeyFunc func(m inbox.OutboundMessage) string

// DefaultKeyFunc buckets outbound sends per (channel, tenant).
func DefaultKeyFunc(m inbox.OutboundMessage) string {
	return "out:" + m.Channel + ":" + m.TenantID.String()
}

// RateLimited decorates an inbox.OutboundChannel with a sliding-window
// rate limit applied BEFORE the carrier call. When the window is
// exhausted it returns inbox.ErrChannelTransient and issues no carrier
// request, so the operator sees a retryable failure and the recipient
// never receives a duplicate. A limiter error is likewise surfaced as
// transient (fail-closed: an unavailable limiter must not silently
// bypass the budget).
type RateLimited struct {
	inner   inbox.OutboundChannel
	limiter RateLimiter
	window  time.Duration
	max     int
	keyFn   KeyFunc
}

// NewRateLimited wraps inner with limiter. window/max define the budget
// (e.g. time.Minute / WHATSAPP_RATE_MAX_PER_MIN). A nil limiter, a
// non-positive max, or a non-positive window disables limiting and
// returns inner unchanged, so misconfiguration fails open to the raw
// sender rather than blocking every send. A nil keyFn falls back to
// DefaultKeyFunc.
func NewRateLimited(inner inbox.OutboundChannel, limiter RateLimiter, window time.Duration, max int, keyFn KeyFunc) inbox.OutboundChannel {
	if limiter == nil || max <= 0 || window <= 0 {
		return inner
	}
	if keyFn == nil {
		keyFn = DefaultKeyFunc
	}
	return &RateLimited{inner: inner, limiter: limiter, window: window, max: max, keyFn: keyFn}
}

// SendMessage implements inbox.OutboundChannel.
func (d *RateLimited) SendMessage(ctx context.Context, m inbox.OutboundMessage) (string, error) {
	allowed, retryAfter, err := d.limiter.Allow(ctx, d.keyFn(m), d.window, d.max)
	if err != nil {
		return "", fmt.Errorf("%w: outbound rate limiter: %v", inbox.ErrChannelTransient, err)
	}
	if !allowed {
		return "", fmt.Errorf("%w: outbound rate limit exceeded, retry after %s", inbox.ErrChannelTransient, retryAfter)
	}
	return d.inner.SendMessage(ctx, m)
}

// Compile-time assertion that RateLimited implements inbox.OutboundChannel.
var _ inbox.OutboundChannel = (*RateLimited)(nil)
