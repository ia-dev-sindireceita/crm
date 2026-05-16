package messenger

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// TenantResolver maps a Messenger page id (entry[].id in the webhook
// envelope) to its owning tenant. The concrete adapter queries
// tenant_channel_associations with channel="messenger" and
// association=<page_id>; the webhook handler treats an unknown page id
// as a silent drop (anti-enumeration) so an attacker who spams the
// endpoint with random page ids cannot distinguish "tenant exists"
// from "tenant exists but flag off".
type TenantResolver interface {
	Resolve(ctx context.Context, pageID string) (uuid.UUID, error)
}

// TenantResolverFunc is a convenience adapter that lets a plain
// closure satisfy TenantResolver. Composition-root wiring uses it to
// translate the postgres sentinel into messenger.ErrUnknownPageID
// without forcing the postgres package to import this one.
type TenantResolverFunc func(ctx context.Context, pageID string) (uuid.UUID, error)

// Resolve implements TenantResolver.
func (f TenantResolverFunc) Resolve(ctx context.Context, pageID string) (uuid.UUID, error) {
	return f(ctx, pageID)
}

// FeatureFlag answers "is the inbound Messenger path enabled for this
// tenant?". Flipping the flag off stops Message materialisation
// immediately while leaving HMAC verification and dedup intact.
type FeatureFlag interface {
	Enabled(ctx context.Context, tenantID uuid.UUID) (bool, error)
}

// Clock decouples time.Now from the timestamp-window check so unit
// tests can pin "now" without sleeping. Production uses systemClock.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

// Now implements Clock.
func (systemClock) Now() time.Time { return time.Now().UTC() }
