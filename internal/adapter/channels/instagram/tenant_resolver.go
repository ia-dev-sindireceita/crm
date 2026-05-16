package instagram

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrUnknownIGBusinessID is returned by TenantResolver implementations
// when no tenant_channel_associations row maps the Instagram Business
// Account id. The handler treats this as a silent drop.
var ErrUnknownIGBusinessID = errors.New("instagram: unknown ig_business_id")

// TenantResolverFunc lets a plain closure satisfy the TenantResolver
// interface. Composition-root wiring uses it to translate the postgres
// sentinel into ErrUnknownIGBusinessID without forcing the postgres
// package to import this one.
type TenantResolverFunc func(ctx context.Context, igBusinessID string) (uuid.UUID, error)

// Resolve implements TenantResolver.
func (f TenantResolverFunc) Resolve(ctx context.Context, igBusinessID string) (uuid.UUID, error) {
	return f(ctx, igBusinessID)
}

// AssociationLookup is the narrow surface NewTenantResolver consumes
// from the postgres store. Keeping the dependency as an interface lets
// unit tests inject an in-memory fake and lets the adapter package
// compile without dragging pgx into its import graph.
//
// Implementations MUST return errors.Is-matchable
// ErrAssociationUnknown when no row maps (channel, association) to a
// tenant. Any other error is propagated as-is.
type AssociationLookup interface {
	Resolve(ctx context.Context, channel, association string) (uuid.UUID, error)
}

// ErrAssociationUnknown is the sentinel an AssociationLookup MUST
// return on a miss. Mirrors the whatsapp adapter's equivalent; the
// composition root translates between the two.
var ErrAssociationUnknown = errors.New("instagram: association unknown")

// pgTenantResolver implements TenantResolver on top of an
// AssociationLookup. Stateless; one per process.
type pgTenantResolver struct {
	lookup AssociationLookup
}

// NewTenantResolver returns a TenantResolver that resolves
// ig_business_id via the supplied lookup. Passing a nil lookup is a
// programming error and panics.
func NewTenantResolver(l AssociationLookup) TenantResolver {
	if l == nil {
		panic("instagram: NewTenantResolver: lookup is nil")
	}
	return &pgTenantResolver{lookup: l}
}

// Resolve implements TenantResolver. A miss is normalised to
// ErrUnknownIGBusinessID; other errors are wrapped with %w so the
// handler's log line stays consistent.
func (r *pgTenantResolver) Resolve(ctx context.Context, igBusinessID string) (uuid.UUID, error) {
	id, err := r.lookup.Resolve(ctx, Channel, igBusinessID)
	if err != nil {
		if errors.Is(err, ErrAssociationUnknown) {
			return uuid.Nil, ErrUnknownIGBusinessID
		}
		return uuid.Nil, fmt.Errorf("instagram: resolve ig_business_id: %w", err)
	}
	return id, nil
}
