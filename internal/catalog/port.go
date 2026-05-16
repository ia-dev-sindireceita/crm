package catalog

import (
	"context"

	"github.com/google/uuid"
)

// ProductRepository is the persistence port for Product.
//
// Reads are tenant-scoped (app_runtime role with RLS). Writes require
// the master_ops role and record the actorID in the audit trail.
//
// Implementations MUST translate:
//   - "no rows"        → ErrNotFound
//   - uuid.Nil tenant  → ErrZeroTenant
type ProductRepository interface {
	// GetByID returns the product for (tenantID, productID), or
	// ErrNotFound.
	GetByID(ctx context.Context, tenantID, productID uuid.UUID) (*Product, error)

	// ListByTenant returns all products for tenantID ordered by
	// created_at ascending.
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*Product, error)

	// SaveProduct inserts or updates the product row. actorID is
	// recorded in the master_ops audit trail. Implementations MUST
	// run inside WithMasterOps so the audit trigger fires.
	SaveProduct(ctx context.Context, p *Product, actorID uuid.UUID) error

	// DeleteProduct removes the product. The FK on product_argument
	// is ON DELETE CASCADE, so arguments disappear with the product.
	// Returns ErrNotFound when no row matched (tenantID, productID).
	DeleteProduct(ctx context.Context, tenantID, productID, actorID uuid.UUID) error
}

// ArgumentRepository is the persistence port for ProductArgument.
//
// Reads are tenant-scoped (app_runtime role with RLS). Writes require
// the master_ops role and record the actorID in the audit trail.
//
// Implementations MUST translate:
//   - "no rows"                                       → ErrNotFound
//   - unique violation on (tenant_id, product_id,
//     scope_type, scope_id)                           → ErrDuplicateArgument
//   - uuid.Nil tenant                                 → ErrZeroTenant
type ArgumentRepository interface {
	// ListByProduct returns all arguments attached to productID for
	// tenantID, in stable order by (scope_type, scope_id, created_at)
	// so callers can fold them deterministically. The resolver runs
	// on top of this output, so the order is not load-bearing for
	// correctness — only for reproducible UI listings.
	ListByProduct(ctx context.Context, tenantID, productID uuid.UUID) ([]*ProductArgument, error)

	// SaveArgument inserts or updates the argument row. actorID is
	// recorded in the master_ops audit trail. Implementations MUST
	// run inside WithMasterOps so the audit trigger fires.
	SaveArgument(ctx context.Context, a *ProductArgument, actorID uuid.UUID) error

	// DeleteArgument removes the argument matching (tenantID,
	// argumentID). Returns ErrNotFound when no row matched.
	DeleteArgument(ctx context.Context, tenantID, argumentID, actorID uuid.UUID) error
}
