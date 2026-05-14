package contacts

import (
	"context"

	"github.com/google/uuid"
)

// Repository is the storage port for Contact aggregates. The concrete
// adapter lives in internal/adapter/db/postgres/contacts.
//
// Every method is tenant-scoped. The Postgres adapter runs each call
// inside postgres.WithTenant, which sets app.tenant_id GUC so the RLS
// policies on contact / contact_channel_identity (migration 0088)
// restrict the visible rows. Callers MUST pass the resolved tenant
// from their request context; passing uuid.Nil yields a clean error,
// not a row-leak.
//
// The port is intentionally small — Save, FindByID,
// FindByChannelIdentity — because PR3 only needs the upsert-by-channel
// use-case. PR4 (inbox listing) and PR6 (webhook receiver) extend it
// when their own use-cases need List / Update / DeleteIdentity.
type Repository interface {
	// Save persists a brand-new Contact along with all its channel
	// identities in a single transaction. Returns
	// ErrChannelIdentityConflict if the UNIQUE(channel, external_id)
	// constraint fires — a different contact already claims one of the
	// identities. Save is not an upsert: calling it twice for the same
	// contact is a programming error.
	Save(ctx context.Context, c *Contact) error

	// FindByID returns the contact with the given id under the tenant
	// scope. Returns ErrNotFound when no row matches (RLS-hidden rows
	// from other tenants collapse to the same sentinel so adversaries
	// cannot tell "exists under another tenant" from "does not exist").
	FindByID(ctx context.Context, tenantID, id uuid.UUID) (*Contact, error)

	// FindByChannelIdentity returns the contact whose
	// contact_channel_identity row matches (channel, externalID) under
	// the given tenant scope. Returns ErrNotFound when no row matches.
	// Does NOT create implicitly — the use-case layer owns the "create
	// if missing" decision so the receiver can name a tenant
	// explicitly.
	FindByChannelIdentity(ctx context.Context, tenantID uuid.UUID, channel, externalID string) (*Contact, error)
}
