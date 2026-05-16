package catalog

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// Product is a per-tenant billable item the IA can pitch in
// conversations. It maps 1:1 to the product row (migration 0098).
//
// Invariants enforced by NewProduct:
//
//  1. tenantID != uuid.Nil — every product belongs to a tenant.
//  2. name is non-empty after trimming whitespace — empty names
//     produce un-renderable rows.
//  3. priceCents >= 0 — the DB CHECK enforces the same floor; a
//     domain rejection produces an actionable error before the
//     INSERT round-trip.
//  4. tags has no blank entries — a tag of "" is meaningless and
//     would bloat the GIN index the W2B resolver may add later.
type Product struct {
	id          uuid.UUID
	tenantID    uuid.UUID
	name        string
	description string
	priceCents  int
	tags        []string
	createdAt   time.Time
	updatedAt   time.Time
}

// NewProduct constructs a Product. The caller supplies `now` so tests
// can pin time without monkey-patching time.Now. Returns
// ErrInvalidProduct (wrapped via fmt.Errorf at the call site if
// desired) on any invariant violation.
func NewProduct(
	tenantID uuid.UUID,
	name, description string,
	priceCents int,
	tags []string,
	now time.Time,
) (*Product, error) {
	if tenantID == uuid.Nil {
		return nil, ErrZeroTenant
	}
	if strings.TrimSpace(name) == "" {
		return nil, ErrInvalidProduct
	}
	if priceCents < 0 {
		return nil, ErrInvalidProduct
	}
	cleaned, err := sanitizeTags(tags)
	if err != nil {
		return nil, err
	}
	return &Product{
		id:          uuid.New(),
		tenantID:    tenantID,
		name:        name,
		description: description,
		priceCents:  priceCents,
		tags:        cleaned,
		createdAt:   now,
		updatedAt:   now,
	}, nil
}

// HydrateProduct reconstructs a Product from durable state. Only
// adapters should call this; it bypasses NewProduct's invariants
// because the database already vetted them.
func HydrateProduct(
	id, tenantID uuid.UUID,
	name, description string,
	priceCents int,
	tags []string,
	createdAt, updatedAt time.Time,
) *Product {
	// Copy the tags slice so adapter callers can mutate their input
	// without leaking changes back into the domain object.
	t := make([]string, len(tags))
	copy(t, tags)
	return &Product{
		id:          id,
		tenantID:    tenantID,
		name:        name,
		description: description,
		priceCents:  priceCents,
		tags:        t,
		createdAt:   createdAt,
		updatedAt:   updatedAt,
	}
}

func (p *Product) ID() uuid.UUID       { return p.id }
func (p *Product) TenantID() uuid.UUID { return p.tenantID }
func (p *Product) Name() string        { return p.name }
func (p *Product) Description() string { return p.description }
func (p *Product) PriceCents() int     { return p.priceCents }

// Tags returns a defensive copy so callers cannot mutate the
// underlying slice and corrupt the aggregate's invariants.
func (p *Product) Tags() []string {
	t := make([]string, len(p.tags))
	copy(t, p.tags)
	return t
}

func (p *Product) CreatedAt() time.Time { return p.createdAt }
func (p *Product) UpdatedAt() time.Time { return p.updatedAt }

// Rename updates the product's name. Returns ErrInvalidProduct on a
// blank name.
func (p *Product) Rename(name string, now time.Time) error {
	if strings.TrimSpace(name) == "" {
		return ErrInvalidProduct
	}
	p.name = name
	p.updatedAt = now
	return nil
}

// SetPrice updates the product's price in cents. Returns
// ErrInvalidProduct for a negative value.
func (p *Product) SetPrice(priceCents int, now time.Time) error {
	if priceCents < 0 {
		return ErrInvalidProduct
	}
	p.priceCents = priceCents
	p.updatedAt = now
	return nil
}

// sanitizeTags trims each tag and rejects blank entries. The
// migration stores tags as text[] with no DB-side validation; the
// domain is the only place that can keep junk tags from accumulating.
func sanitizeTags(tags []string) ([]string, error) {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		trimmed := strings.TrimSpace(t)
		if trimmed == "" {
			return nil, ErrInvalidProduct
		}
		out = append(out, trimmed)
	}
	return out, nil
}
