package master

import (
	"context"
	"errors"

	billingpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/billing"
	"github.com/pericles-luz/crm/internal/billing"
	masterweb "github.com/pericles-luz/crm/internal/web/master"
)

var errNilBillingStore = errors.New("master/postgres: billing store is nil")

// Compile-time assertion.
var _ masterweb.PlanLister = (*PlanListerShim)(nil)

// PlanListerShim adapts billing.PlanCatalog.ListPlans to the masterweb.PlanLister
// interface. The billing.Store already reads from the runtime pool without
// RLS (plan table has no row-level security), so this is a thin delegation.
type PlanListerShim struct {
	store *billingpg.Store
}

// NewPlanListerShim wraps an existing billing.Store.
func NewPlanListerShim(store *billingpg.Store) (*PlanListerShim, error) {
	if store == nil {
		return nil, errNilBillingStore
	}
	return &PlanListerShim{store: store}, nil
}

// List delegates to billing.Store.ListPlans.
func (p *PlanListerShim) List(ctx context.Context) ([]billing.Plan, error) {
	return p.store.ListPlans(ctx)
}
