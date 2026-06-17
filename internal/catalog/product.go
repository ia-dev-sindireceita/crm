package catalog

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// MaxCategoryLen caps Product.Category at the domain layer. The
// column itself is text without a length cap; this floor matches the
// MaxTagLen ceiling on tags so categories stay sidebar-friendly.
const MaxCategoryLen = 64

// Product is a per-tenant billable item the IA can pitch in
// conversations. It maps 1:1 to the product row (migrations 0098,
// 0118).
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
//
// Category is optional; an empty string means "no category" and the
// sidebar renders the product under a "Sem categoria" bucket. Set
// after construction via SetCategory or hydrated from storage via
// HydrateProductFull.
type Product struct {
	id          uuid.UUID
	tenantID    uuid.UUID
	name        string
	description string
	priceCents  int
	tags        []string
	category    string
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

// HydrateProduct reconstructs a Product from durable state without a
// category. Kept for legacy callers; the Postgres adapter uses
// HydrateProductFull so the category column round-trips.
func HydrateProduct(
	id, tenantID uuid.UUID,
	name, description string,
	priceCents int,
	tags []string,
	createdAt, updatedAt time.Time,
) *Product {
	return HydrateProductFull(id, tenantID, name, description, priceCents,
		tags, "", createdAt, updatedAt)
}

// HydrateProductFull reconstructs a Product from durable state
// including category. Only adapters should call this; it bypasses
// NewProduct's invariants because the database already vetted them.
func HydrateProductFull(
	id, tenantID uuid.UUID,
	name, description string,
	priceCents int,
	tags []string,
	category string,
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
		category:    category,
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

// Category returns the product's category, or "" when none is set.
func (p *Product) Category() string { return p.category }

// SetCategory updates the product's category. Empty input clears the
// category (no validation error). Categories longer than
// MaxCategoryLen are rejected with ErrInvalidProduct so the sidebar
// label stays renderable.
func (p *Product) SetCategory(category string, now time.Time) error {
	trimmed := strings.TrimSpace(category)
	if len(trimmed) > MaxCategoryLen {
		return ErrInvalidProduct
	}
	p.category = trimmed
	p.updatedAt = now
	return nil
}

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
