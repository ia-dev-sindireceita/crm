package campaigns

import (
	"context"
	"sort"
	"sync"

	"github.com/google/uuid"
)

// InMemoryRepository is a tenant-scoped, race-safe Repository for tests
// that exercise consumers of the Campaign aggregate without spinning up
// Postgres. It mirrors the documented adapter semantics:
//
//   - GetBySlug returns ErrNotFound for both missing rows and slugs
//     owned by other tenants (no cross-tenant probe).
//   - RecordClick is idempotent on ClickID: a second call with the same
//     ClickID returns the original row and does not double-insert.
//   - LinkContactToCampaign mutates the existing row in place and
//     errors with ErrNotFound when no click matches.
//
// Production wiring uses the pgx-backed Store under
// internal/adapter/db/postgres/campaigns; this type exists strictly to
// keep use-case tests off the database.
type InMemoryRepository struct {
	mu sync.Mutex
	// campaigns keyed by (tenantID, normalised slug).
	campaigns map[campaignKey]*Campaign
	// clicks keyed by (tenantID, clickID) — UNIQUE per the adapter's
	// idempotency contract.
	clicks map[clickKey]*CampaignClick
}

type campaignKey struct {
	tenant uuid.UUID
	slug   string
}

type clickKey struct {
	tenant  uuid.UUID
	clickID string
}

// Compile-time guard: InMemoryRepository satisfies the domain port so a
// drift in port signatures fails the build of this file before any test
// notices.
var _ Repository = (*InMemoryRepository)(nil)

// NewInMemoryRepository returns an empty repository ready to use. Safe
// for concurrent callers.
func NewInMemoryRepository() *InMemoryRepository {
	return &InMemoryRepository{
		campaigns: map[campaignKey]*Campaign{},
		clicks:    map[clickKey]*CampaignClick{},
	}
}

// CreateCampaign stores c indexed by (tenantID, slug). A duplicate slug
// under the same tenant returns ErrSlugAlreadyExists to match the
// adapter's UNIQUE-constraint translation.
func (r *InMemoryRepository) CreateCampaign(_ context.Context, c *Campaign) error {
	if c == nil {
		return ErrInvalidCampaign
	}
	if c.TenantID == uuid.Nil {
		return ErrInvalidTenant
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := campaignKey{tenant: c.TenantID, slug: c.Slug}
	if _, exists := r.campaigns[key]; exists {
		return ErrSlugAlreadyExists
	}
	cp := *c
	r.campaigns[key] = &cp
	return nil
}

// GetBySlug returns the campaign by tenant + normalised slug. A
// not-found row (or any row hidden by tenant scope) collapses to
// ErrNotFound — the caller cannot tell the two cases apart.
func (r *InMemoryRepository) GetBySlug(_ context.Context, tenantID uuid.UUID, slug string) (*Campaign, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	norm, err := NormalizeSlug(slug)
	if err != nil {
		return nil, ErrNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.campaigns[campaignKey{tenant: tenantID, slug: norm}]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *c
	return &cp, nil
}

// RecordClick persists click idempotently on ClickID. A duplicate insert
// returns the pre-existing row with no mutation.
func (r *InMemoryRepository) RecordClick(_ context.Context, click *CampaignClick) (*CampaignClick, error) {
	if click == nil {
		return nil, ErrInvalidClickID
	}
	if click.TenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	if click.CampaignID == uuid.Nil {
		return nil, ErrInvalidCampaign
	}
	if click.ClickID == "" {
		return nil, ErrInvalidClickID
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := clickKey{tenant: click.TenantID, clickID: click.ClickID}
	if existing, ok := r.clicks[key]; ok {
		cp := *existing
		return &cp, nil
	}
	stored := *click
	r.clicks[key] = &stored
	out := stored
	return &out, nil
}

// LinkContactToCampaign sets ContactID on the click row identified by
// (tenantID, clickID). ErrNotFound when the click is unknown under the
// tenant scope.
func (r *InMemoryRepository) LinkContactToCampaign(_ context.Context, tenantID uuid.UUID, clickID string, contactID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return ErrInvalidTenant
	}
	if clickID == "" {
		return ErrInvalidClickID
	}
	if contactID == uuid.Nil {
		return ErrInvalidCampaign
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.clicks[clickKey{tenant: tenantID, clickID: clickID}]
	if !ok {
		return ErrNotFound
	}
	cid := contactID
	existing.ContactID = &cid
	return nil
}

// DumpClicksForTest returns a snapshot copy of every persisted
// CampaignClick row across all tenants. Test-only accessor — the
// production *postgres.Store has no analogue. Lives on the fake so
// handler tests that need to assert on Meta / IP / UA fields do not
// have to chain LinkContactToCampaign probes.
func (r *InMemoryRepository) DumpClicksForTest() []*CampaignClick {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*CampaignClick, 0, len(r.clicks))
	for _, c := range r.clicks {
		cp := *c
		out = append(out, &cp)
	}
	return out
}

// ListByTenant returns every campaign for the tenant, newest-first by
// CreatedAt, matching the adapter ordering.
func (r *InMemoryRepository) ListByTenant(_ context.Context, tenantID uuid.UUID) ([]*Campaign, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Campaign, 0)
	for k, c := range r.campaigns {
		if k.tenant != tenantID {
			continue
		}
		cp := *c
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// StatsByTenant aggregates clicks + attributions per campaign id for
// the tenant. Campaigns with zero clicks are absent from the map,
// matching the adapter contract (callers MUST treat a missing key as
// the zero value CampaignStats{}).
func (r *InMemoryRepository) StatsByTenant(_ context.Context, tenantID uuid.UUID) (map[uuid.UUID]CampaignStats, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[uuid.UUID]CampaignStats{}
	for _, click := range r.clicks {
		if click.TenantID != tenantID {
			continue
		}
		s := out[click.CampaignID]
		s.Clicks++
		if click.ContactID != nil {
			s.Attributions++
		}
		out[click.CampaignID] = s
	}
	return out, nil
}

// ListClicks returns at most limit click rows for the campaign under
// the tenant scope, newest-first by CreatedAt. A non-positive limit
// collapses to defaultListClicksLimit to mirror the pgx adapter's
// bounded SQL (the adapter never returns an unbounded ledger).
func (r *InMemoryRepository) ListClicks(_ context.Context, tenantID, campaignID uuid.UUID, limit int) ([]*CampaignClick, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	if campaignID == uuid.Nil {
		return nil, ErrInvalidCampaign
	}
	if limit <= 0 {
		limit = defaultListClicksLimit
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*CampaignClick, 0)
	for _, click := range r.clicks {
		if click.TenantID != tenantID || click.CampaignID != campaignID {
			continue
		}
		cp := *click
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// defaultListClicksLimit mirrors the bound the pgx adapter uses when
// the caller passes a non-positive limit. Lives here so the in-memory
// fake stays observably consistent with production behaviour.
const defaultListClicksLimit = 50
