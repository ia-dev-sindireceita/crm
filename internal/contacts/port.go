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
// The port grew over the Fase 1 stack: Save, FindByID,
// FindByChannelIdentity for the upsert-by-channel use-case (PR3), then
// List + Update for the contacts management surface (SIN-64976, child of
// SIN-64962) — list/search/detail/edit. Channel identities are NOT
// edited through Update; they follow the existing identity-split flow
// (internal/contacts/identity_port.go).
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

	// List returns one page of contacts under the tenant scope plus the
	// total number of contacts matching the filter (ignoring pagination)
	// so the caller can render "showing N of total". The filter's Query
	// is matched case-insensitively against the contact display name and
	// the external ids of its channel identities (phone/email). Results
	// are ordered deterministically (display name, then id) so paging is
	// stable across calls. A uuid.Nil tenant yields a clean error, never a
	// cross-tenant row leak. An empty page (offset past the end, or no
	// match) is (nil, total, nil), not an error.
	List(ctx context.Context, tenantID uuid.UUID, f ListFilter) (items []*Contact, total int, err error)

	// Update persists the editable fields of an existing contact
	// (currently the display name) under the tenant scope. It does NOT
	// touch channel identities — those are managed through the
	// identity-split flow. Returns ErrNotFound when no row matches the
	// (tenant, id) pair, so the caller maps that to a 404 instead of a
	// silent no-op. The caller MUST mutate the aggregate through its
	// methods (e.g. Contact.Rename) before calling Update so the
	// invariants and UpdatedAt stamp are coherent.
	Update(ctx context.Context, c *Contact) error
}
