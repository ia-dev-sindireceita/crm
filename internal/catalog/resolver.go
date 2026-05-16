package catalog

import (
	"context"
	"sort"

	"github.com/google/uuid"
)

// ArgumentLister is the slice of ArgumentRepository the resolver
// actually consumes. Accepting a small interface (rather than the
// full ArgumentRepository) keeps the resolver test-friendly without
// stubbing methods it never calls.
type ArgumentLister interface {
	ListByProduct(ctx context.Context, tenantID, productID uuid.UUID) ([]*ProductArgument, error)
}

// Resolver picks the product arguments that apply under a runtime
// Scope, ordered from most-specific to least-specific. The cascade
// matches migration 0098 and the W2A policy resolver: when channel,
// team, and tenant anchors all have a matching argument, the channel
// argument comes first, then team, then tenant.
type Resolver struct {
	args ArgumentLister
}

// NewResolver wires the resolver against an ArgumentLister.
func NewResolver(args ArgumentLister) *Resolver {
	return &Resolver{args: args}
}

// ResolveArguments returns the arguments attached to productID that
// apply under `scope`, ordered channel > team > tenant. The
// tenant-scoped argument always qualifies (it is the catch-all); the
// team and channel arguments only qualify when the scope's TeamID /
// ChannelID matches the anchor's id.
//
// The returned slice contains at most one argument per scope_type
// because (tenant_id, product_id, scope_type, scope_id) is UNIQUE in
// the schema. Callers can either fold the whole slice into a prompt
// or take the first (most-specific) entry — both modes are
// supported because the order is stable.
//
// Returns ErrZeroTenant when tenantID is uuid.Nil and
// ErrInvalidArgument when productID is uuid.Nil. An empty Scope is
// legal — it surfaces only the tenant-scoped argument.
func (r *Resolver) ResolveArguments(
	ctx context.Context,
	tenantID, productID uuid.UUID,
	scope Scope,
) ([]*ProductArgument, error) {
	if tenantID == uuid.Nil {
		return nil, ErrZeroTenant
	}
	if productID == uuid.Nil {
		return nil, ErrInvalidArgument
	}

	all, err := r.args.ListByProduct(ctx, tenantID, productID)
	if err != nil {
		return nil, err
	}

	matches := make([]*ProductArgument, 0, len(all))
	for _, a := range all {
		if a == nil {
			continue
		}
		if !scope.matches(a.Anchor()) {
			continue
		}
		matches = append(matches, a)
	}

	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].Anchor().Type.specificity() >
			matches[j].Anchor().Type.specificity()
	})
	return matches, nil
}
