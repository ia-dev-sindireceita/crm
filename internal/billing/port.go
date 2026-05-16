package billing

import (
	"context"

	"github.com/google/uuid"
)

// PlanCatalog is the read-only port for the global plan catalogue.
//
// Implementations MUST translate "no rows" to ErrNotFound so callers can
// match with errors.Is without importing pgx. The plan table has no RLS
// (it is non-tenanted), so adapters may use either the runtime or admin
// pool; the runtime pool is preferred for reads.
type PlanCatalog interface {
	// ListPlans returns all plans ordered by price_cents_brl ascending.
	ListPlans(ctx context.Context) ([]Plan, error)

	// GetBySlug returns the plan with the given slug, or ErrNotFound.
	GetBySlug(ctx context.Context, slug string) (Plan, error)
}

// SubscriptionRepository is the persistence port for Subscription.
//
// Reads are tenant-scoped (app_runtime role with RLS). Writes require the
// master_ops role and record the actorID in the audit trail.
//
// Implementations MUST translate:
//   - "no rows"                                  → ErrNotFound
//   - unique violation on (tenant_id) WHERE status='active' → ErrInvalidTransition
type SubscriptionRepository interface {
	// GetByTenant returns the active subscription for tenantID, or ErrNotFound.
	// Returns ErrZeroTenant for uuid.Nil tenantID.
	GetByTenant(ctx context.Context, tenantID uuid.UUID) (*Subscription, error)

	// SaveSubscription inserts or updates the subscription row. actorID is
	// recorded in the master_ops audit trail. Implementations MUST run inside
	// WithMasterOps so the audit trigger fires.
	SaveSubscription(ctx context.Context, s *Subscription, actorID uuid.UUID) error
}

// InvoiceRepository is the persistence port for Invoice.
//
// Reads are tenant-scoped (app_runtime role with RLS). Writes require the
// master_ops role and record the actorID in the audit trail.
//
// Implementations MUST translate:
//   - "no rows"                                                   → ErrNotFound
//   - unique violation on (tenant_id, period_start) WHERE state <> 'cancelled_by_master' → ErrInvoiceAlreadyExists
type InvoiceRepository interface {
	// GetByID returns the invoice for (tenantID, invoiceID), or ErrNotFound.
	// Returns ErrZeroTenant for uuid.Nil tenantID.
	GetByID(ctx context.Context, tenantID, invoiceID uuid.UUID) (*Invoice, error)

	// ListByTenant returns all invoices for tenantID ordered by period_start DESC.
	// Returns ErrZeroTenant for uuid.Nil tenantID.
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*Invoice, error)

	// SaveInvoice inserts or updates the invoice row. actorID is recorded in
	// the master_ops audit trail. Implementations MUST run inside WithMasterOps
	// so the audit trigger fires.
	SaveInvoice(ctx context.Context, inv *Invoice, actorID uuid.UUID) error
}
