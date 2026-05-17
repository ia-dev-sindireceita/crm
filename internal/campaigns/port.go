package campaigns

import (
	"context"

	"github.com/google/uuid"
)

// Repository is the storage port for the Campaign + CampaignClick
// aggregates. The concrete pgx-backed adapter lives in
// internal/adapter/db/postgres/campaigns; the use-case layer depends
// only on this interface so unit tests can substitute an in-memory
// fake (see InMemoryRepository).
//
// Every method is tenant-scoped — the Postgres adapter runs each call
// inside postgres.WithTenant so the RLS policies on campaigns /
// campaign_clicks (migration 0102) restrict the visible rows. Callers
// MUST pass the resolved tenant from their request context; passing
// uuid.Nil yields ErrInvalidTenant, not a row-leak.
type Repository interface {
	// CreateCampaign persists a new Campaign row. Returns
	// ErrSlugAlreadyExists if the UNIQUE (tenant_id, slug) constraint
	// fires — the use-case layer maps this to a 409. The caller MUST
	// construct the Campaign via NewCampaign so invariants hold; the
	// adapter does not re-validate the slug/redirect.
	CreateCampaign(ctx context.Context, c *Campaign) error

	// GetBySlug returns the Campaign with (tenantID, slug). slug is
	// normalised internally so callers may pass the raw URL component
	// untouched. Returns ErrNotFound when no row matches (including
	// the RLS-hidden case so adversaries cannot probe other tenants).
	GetBySlug(ctx context.Context, tenantID uuid.UUID, slug string) (*Campaign, error)

	// RecordClick persists a CampaignClick idempotently on ClickID.
	// If a row with the same ClickID already exists, the existing row
	// is returned WITHOUT error and WITHOUT a duplicate insert. The
	// adapter MUST guarantee the same outcome under concurrent
	// retries (two browsers double-tapping at once both observe the
	// first row).
	//
	// Returns the persisted (or pre-existing) row so callers can chain
	// off the canonical CreatedAt.
	RecordClick(ctx context.Context, click *CampaignClick) (*CampaignClick, error)

	// LinkContactToCampaign sets contact_id on the click identified by
	// clickID. Used when the visitor identifies AFTER the click
	// landed (e.g. fills out a lead form on the redirect target).
	// Returns ErrNotFound if the click_id does not exist under the
	// tenant scope.
	LinkContactToCampaign(ctx context.Context, tenantID uuid.UUID, clickID string, contactID uuid.UUID) error

	// ListByTenant returns every campaign under the tenant scope,
	// newest-first by CreatedAt. The list view is small (marketer
	// builds a handful per quarter); no pagination today — when it
	// becomes one we add a cursor argument, not an offset.
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*Campaign, error)
}
