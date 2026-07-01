package channels

import (
	"context"

	"github.com/google/uuid"
)

// Repository is the storage port for Channel aggregates. The concrete
// adapter lives in internal/adapter/db/postgres/channels.
//
// Every method is tenant-scoped. The Postgres adapter runs each call
// inside postgres.WithTenant, which sets the app.tenant_id GUC so the RLS
// policies on tenant_channels (migration 0128) restrict the visible rows.
// Callers MUST pass the resolved tenant from their request context;
// passing uuid.Nil yields a clean error, not a row leak.
type Repository interface {
	// List returns every channel instance for the tenant, ordered
	// deterministically (channel_key, then external_id, then id) so the
	// picker is stable across calls. An empty tenant yields no rows; a
	// uuid.Nil tenant is a clean error, never a cross-tenant leak.
	List(ctx context.Context, tenantID uuid.UUID) ([]*Channel, error)

	// Create persists a brand-new channel instance. Returns
	// ErrChannelConflict if the INSERT would violate
	// UNIQUE(tenant_id, channel_key, external_id). Create is not an
	// upsert: the caller owns the "already exists" decision.
	Create(ctx context.Context, c *Channel) error

	// Rename updates the display name of the channel identified by
	// (tenantID, id). Returns ErrNotFound when no row matches the scope
	// so the caller maps it to a 404 rather than a silent no-op. The new
	// name MUST be non-empty after trimming (validated by the caller via
	// Channel.Rename, and again defensively here).
	Rename(ctx context.Context, tenantID, id uuid.UUID, displayName string) error

	// SetActive flips the is_active flag of the channel identified by
	// (tenantID, id). Returns ErrNotFound when no row matches the scope.
	SetActive(ctx context.Context, tenantID, id uuid.UUID, active bool) error

	// SetRestricted flips the restricted flag of the channel identified
	// by (tenantID, id). Returns ErrNotFound when no row matches the
	// scope. The flag is the per-resource access-policy input consumed by
	// AccessService: false = open to the whole tenant, true = limited to
	// the users holding an explicit channel_access grant (plus the
	// gerente override). Toggling it never touches the stored grant
	// roster, so a channel can be flipped open→restricted→open without
	// re-authoring who has access.
	SetRestricted(ctx context.Context, tenantID, id uuid.UUID, restricted bool) error

	// Get returns the channel with the given id under the tenant scope,
	// or ErrNotFound when no row matches.
	Get(ctx context.Context, tenantID, id uuid.UUID) (*Channel, error)
}

// ChannelAccessPolicy answers per-user channel authorization questions.
// The concrete adapter (internal/adapter/db/postgres/channels) reads the
// channel_access table under tenant RLS.
//
// NOTE on the signature: the SIN-66389 ticket sketched these methods as
// CanAccessChannel(ctx, userID, channelID) and
// ListAccessibleChannelIDs(ctx, userID) — without a tenant. Every other
// repository port in this codebase takes an explicit tenantID (see
// contacts.Repository, funnel.*Repository), and the RLS-scoped adapter
// physically cannot run a query without first setting app.tenant_id; a
// tenant-less read would either deny every row or, if the GUC leaked from
// a prior request, read the wrong tenant. So the tenant is threaded
// explicitly here. This port is a Phase-1 stub with no consumers yet
// (SIN-66389 BLOCKS P2–P4), so tightening the signature now is free and
// reversible — flagged for CTO review on the task thread.
type ChannelAccessPolicy interface {
	// CanAccessChannel reports whether userID may act on the channel
	// instance channelID within tenantID. A non-existent grant returns
	// (false, nil), not an error. Note this answers only the explicit
	// grant question; callers layer the channel's Restricted flag and
	// role rules on top in later phases.
	CanAccessChannel(ctx context.Context, tenantID, userID, channelID uuid.UUID) (bool, error)

	// ListAccessibleChannelIDs returns the ids of every channel instance
	// userID has an explicit grant for within tenantID, ordered for
	// determinism. An empty slice (nil error) is the natural "no grants"
	// outcome.
	ListAccessibleChannelIDs(ctx context.Context, tenantID, userID uuid.UUID) ([]uuid.UUID, error)
}

// ChannelResolver maps an inbound identity to the tenant's channel
// instance id. It is the routing seam for SIN-66378 P4: the
// receive-inbound use case calls it so a new conversation references the
// tenant_channels row rather than the bare carrier string, which is what
// lets two numbers of the same carrier live side by side without
// colliding.
//
// The concrete adapter (internal/adapter/db/postgres/channels) reads
// tenant_channels under tenant RLS. Passing uuid.Nil for the tenant is a
// clean error, never a cross-tenant leak.
type ChannelResolver interface {
	// ResolveChannelID returns the id of the tenant channel instance for
	// the given carrier (channelKey, e.g. "whatsapp") that received the
	// message on externalID (the tenant-side destination address). An
	// empty externalID means the carrier did not surface a receiver
	// address; the resolver then falls back to the tenant's instance for
	// that carrier (single-instance tenants, or the migration-0128
	// placeholder), preferring an active row and ordering deterministically
	// so the choice is stable. Returns uuid.Nil (nil error) when no
	// instance matches, so the caller records the conversation with a NULL
	// channel_id rather than failing the inbound delivery.
	ResolveChannelID(ctx context.Context, tenantID uuid.UUID, channelKey, externalID string) (uuid.UUID, error)
}
