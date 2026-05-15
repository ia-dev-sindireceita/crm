package whatsapp

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// TenantResolverFunc is a convenience adapter that lets a plain
// closure satisfy the TenantResolver interface. Composition-root
// wiring uses it to translate the postgres-side sentinel into
// whatsapp.ErrUnknownPhoneNumberID without forcing the postgres
// package to import this one. Example wire-up:
//
//	resolver := whatsapp.TenantResolverFunc(func(ctx context.Context, pn string) (uuid.UUID, error) {
//	    id, err := lookup.Resolve(ctx, whatsapp.Channel, pn)
//	    if errors.Is(err, postgres.ErrAssociationUnknown) {
//	        return uuid.Nil, whatsapp.ErrUnknownPhoneNumberID
//	    }
//	    return id, err
//	})
type TenantResolverFunc func(ctx context.Context, phoneNumberID string) (uuid.UUID, error)

// Resolve implements TenantResolver.
func (f TenantResolverFunc) Resolve(ctx context.Context, phoneNumberID string) (uuid.UUID, error) {
	return f(ctx, phoneNumberID)
}

// AssociationLookup is the narrow surface NewTenantResolver consumes
// from the postgres store. Keeping the dependency as an interface
// lets unit tests inject an in-memory fake and lets the adapter
// package compile without dragging pgx into its import graph.
//
// Implementations MUST return an errors.Is-matchable
// whatsapp.ErrAssociationUnknown when no row maps (channel,
// association) to a tenant. Any other error is propagated as-is.
type AssociationLookup interface {
	Resolve(ctx context.Context, channel, association string) (uuid.UUID, error)
}

// ErrAssociationUnknown is the sentinel an AssociationLookup MUST
// return on a miss. Defined here so the postgres adapter can import
// it without inverting the adapter→adapter dependency direction; the
// composition root translates between this sentinel and the package-
// internal ErrUnknownPhoneNumberID.
var ErrAssociationUnknown = errors.New("whatsapp: association unknown")

// pgTenantResolver implements TenantResolver on top of an
// AssociationLookup. The composition root constructs one of these per
// process; the resolver is stateless.
type pgTenantResolver struct {
	lookup AssociationLookup
}

// NewTenantResolver returns a TenantResolver that resolves
// phone_number_id via the supplied lookup. Passing a nil lookup is a
// programming error and panics.
func NewTenantResolver(l AssociationLookup) TenantResolver {
	if l == nil {
		panic("whatsapp: NewTenantResolver: lookup is nil")
	}
	return &pgTenantResolver{lookup: l}
}

// Resolve implements TenantResolver. A miss is normalised to
// ErrUnknownPhoneNumberID; every other error is wrapped with %w so the
// handler's log line is consistent.
func (r *pgTenantResolver) Resolve(ctx context.Context, phoneNumberID string) (uuid.UUID, error) {
	id, err := r.lookup.Resolve(ctx, Channel, phoneNumberID)
	if err != nil {
		if errors.Is(err, ErrAssociationUnknown) {
			return uuid.Nil, ErrUnknownPhoneNumberID
		}
		return uuid.Nil, fmt.Errorf("whatsapp: resolve phone_number_id: %w", err)
	}
	return id, nil
}
