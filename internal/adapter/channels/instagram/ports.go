package instagram

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// TenantResolver maps an Instagram Business Account id (the
// entry[].id / recipient.id in Meta's webhook envelope) to its owning
// tenant. Production wires a postgres-backed lookup against
// tenant_channel_associations where channel = "instagram"; the handler
// treats ErrUnknownIGBusinessID as a silent drop.
type TenantResolver interface {
	Resolve(ctx context.Context, igBusinessID string) (uuid.UUID, error)
}

// FeatureFlag answers "is the inbound Instagram path enabled for this
// tenant?". The default EnvFeatureFlag in this package reads
// FEATURE_INSTAGRAM_ENABLED + FEATURE_INSTAGRAM_TENANTS. A nil flag
// fails closed.
type FeatureFlag interface {
	Enabled(ctx context.Context, tenantID uuid.UUID) (bool, error)
}

// RateLimiter is the per-key counter port. The Instagram adapter keys
// limits on ig_business_id (NOT tenant_id) so an unknown-account flood
// can be absorbed before tenant resolution runs.
type RateLimiter interface {
	Allow(ctx context.Context, key string, window time.Duration, max int) (allowed bool, retryAfter time.Duration, err error)
}

// MediaScanPublisher is the port the handler uses to fan-out
// `media.scan.requested` envelopes (F2-05 SubjectRequested). One Publish
// call per attachment. Implementations marshal the envelope and write
// to the NATS subject; a nil publisher fails closed (handler logs a
// warn and counts the drop, but the message itself still persists with
// the placeholder body).
type MediaScanPublisher interface {
	PublishScanRequest(ctx context.Context, tenantID, messageID uuid.UUID, key string) error
}

// Clock decouples time.Now from the timestamp-window and 24h outbound
// window checks so unit tests can pin "now" without sleeping.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

// Now implements Clock.
func (systemClock) Now() time.Time { return time.Now().UTC() }
