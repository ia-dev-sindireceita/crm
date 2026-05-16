// Package billing (adapter) implements billing.PlanCatalog,
// billing.SubscriptionRepository, and billing.InvoiceRepository against
// Postgres. Reads route through the runtime pool inside WithTenant so RLS
// gates tenant visibility. Writes route through the master pool inside
// WithMasterOps so the audit trigger fires on every mutation.
package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/billing"
)

var (
	_ billing.PlanCatalog            = (*Store)(nil)
	_ billing.SubscriptionRepository = (*Store)(nil)
	_ billing.InvoiceRepository      = (*Store)(nil)
)

// Store implements the three billing ports against Postgres.
//
// runtimePool connects as app_runtime (RLS applies; BYPASSRLS=false). It is
// used for tenant-scoped reads on subscription and invoice, and for global
// reads on plan (which has no RLS).
//
// masterPool connects as app_master_ops (BYPASSRLS=true; audit trigger fires).
// It is used for all writes.
type Store struct {
	runtimePool *pgxpool.Pool
	masterPool  *pgxpool.Pool
}

// New constructs a Store. Both pools must be non-nil.
func New(runtimePool, masterPool *pgxpool.Pool) (*Store, error) {
	if runtimePool == nil || masterPool == nil {
		return nil, postgresadapter.ErrNilPool
	}
	return &Store{runtimePool: runtimePool, masterPool: masterPool}, nil
}

// ---------------------------------------------------------------------------
// PlanCatalog
// ---------------------------------------------------------------------------

// ListPlans returns all plans ordered by price_cents_brl ascending.
// The plan table has no RLS, so this query uses the runtime pool directly
// without a tenant scope.
func (s *Store) ListPlans(ctx context.Context) ([]billing.Plan, error) {
	rows, err := s.runtimePool.Query(ctx,
		`SELECT id, slug, name, price_cents_brl, monthly_token_quota, created_at, updated_at
		   FROM plan ORDER BY price_cents_brl ASC`)
	if err != nil {
		return nil, fmt.Errorf("billing/postgres: list plans: %w", err)
	}
	defer rows.Close()
	var plans []billing.Plan
	for rows.Next() {
		var p billing.Plan
		if err := rows.Scan(&p.ID, &p.Slug, &p.Name, &p.PriceCentsBRL, &p.MonthlyTokenQuota, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("billing/postgres: scan plan: %w", err)
		}
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("billing/postgres: list plans: %w", err)
	}
	return plans, nil
}

// GetBySlug returns the plan with the given slug, or billing.ErrNotFound.
func (s *Store) GetBySlug(ctx context.Context, slug string) (billing.Plan, error) {
	var p billing.Plan
	err := s.runtimePool.QueryRow(ctx,
		`SELECT id, slug, name, price_cents_brl, monthly_token_quota, created_at, updated_at
		   FROM plan WHERE slug = $1`, slug,
	).Scan(&p.ID, &p.Slug, &p.Name, &p.PriceCentsBRL, &p.MonthlyTokenQuota, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return billing.Plan{}, billing.ErrNotFound
		}
		return billing.Plan{}, fmt.Errorf("billing/postgres: get plan by slug: %w", err)
	}
	return p, nil
}

// GetPlanByID returns the plan with the given UUID, or billing.ErrNotFound.
func (s *Store) GetPlanByID(ctx context.Context, id uuid.UUID) (billing.Plan, error) {
	if id == uuid.Nil {
		return billing.Plan{}, billing.ErrNotFound
	}
	var p billing.Plan
	err := s.runtimePool.QueryRow(ctx,
		`SELECT id, slug, name, price_cents_brl, monthly_token_quota, created_at, updated_at
		   FROM plan WHERE id = $1`, id,
	).Scan(&p.ID, &p.Slug, &p.Name, &p.PriceCentsBRL, &p.MonthlyTokenQuota, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return billing.Plan{}, billing.ErrNotFound
		}
		return billing.Plan{}, fmt.Errorf("billing/postgres: get plan by id: %w", err)
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// SubscriptionRepository
// ---------------------------------------------------------------------------

// GetByTenant returns the active subscription for tenantID, or
// billing.ErrNotFound. Returns billing.ErrZeroTenant for uuid.Nil.
func (s *Store) GetByTenant(ctx context.Context, tenantID uuid.UUID) (*billing.Subscription, error) {
	if tenantID == uuid.Nil {
		return nil, billing.ErrZeroTenant
	}
	var sub *billing.Subscription
	err := postgresadapter.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		got, err := scanSubscription(tx.QueryRow(ctx,
			`SELECT id, tenant_id, plan_id, status,
			        current_period_start, current_period_end,
			        created_at, updated_at
			   FROM subscription
			  WHERE tenant_id = $1 AND status = 'active'`, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return billing.ErrNotFound
			}
			return fmt.Errorf("billing/postgres: get subscription: %w", err)
		}
		sub = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sub, nil
}

// SaveSubscription inserts or updates the subscription row. The upsert key is
// the subscription's primary key (id); status/period fields are updated on
// conflict. actorID is recorded in the master_ops audit trail.
func (s *Store) SaveSubscription(ctx context.Context, sub *billing.Subscription, actorID uuid.UUID) error {
	if sub == nil {
		return fmt.Errorf("billing/postgres: subscription is nil")
	}
	return postgresadapter.WithMasterOps(ctx, s.masterPool, actorID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO subscription
			  (id, tenant_id, plan_id, status,
			   current_period_start, current_period_end,
			   created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (id) DO UPDATE SET
			  status               = EXCLUDED.status,
			  plan_id              = EXCLUDED.plan_id,
			  current_period_start = EXCLUDED.current_period_start,
			  current_period_end   = EXCLUDED.current_period_end,
			  updated_at           = EXCLUDED.updated_at`,
			sub.ID(), sub.TenantID(), sub.PlanID(), string(sub.Status()),
			sub.CurrentPeriodStart(), sub.CurrentPeriodEnd(),
			sub.CreatedAt(), sub.UpdatedAt(),
		)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return billing.ErrInvalidTransition
			}
			return fmt.Errorf("billing/postgres: save subscription: %w", err)
		}
		return nil
	})
}

func scanSubscription(row pgx.Row) (*billing.Subscription, error) {
	var (
		id                 uuid.UUID
		tenantID           uuid.UUID
		planID             uuid.UUID
		status             string
		currentPeriodStart time.Time
		currentPeriodEnd   time.Time
		createdAt          time.Time
		updatedAt          time.Time
	)
	if err := row.Scan(&id, &tenantID, &planID, &status,
		&currentPeriodStart, &currentPeriodEnd,
		&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	return billing.HydrateSubscription(
		id, tenantID, planID,
		billing.SubscriptionStatus(status),
		currentPeriodStart, currentPeriodEnd,
		createdAt, updatedAt,
	), nil
}

// ---------------------------------------------------------------------------
// InvoiceRepository
// ---------------------------------------------------------------------------

// GetByID returns the invoice for (tenantID, invoiceID), or billing.ErrNotFound.
func (s *Store) GetByID(ctx context.Context, tenantID, invoiceID uuid.UUID) (*billing.Invoice, error) {
	if tenantID == uuid.Nil {
		return nil, billing.ErrZeroTenant
	}
	var inv *billing.Invoice
	err := postgresadapter.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		got, err := scanInvoice(tx.QueryRow(ctx,
			`SELECT id, tenant_id, subscription_id,
			        period_start, period_end,
			        amount_cents_brl, state, cancelled_reason,
			        created_at, updated_at
			   FROM invoice
			  WHERE id = $1 AND tenant_id = $2`, invoiceID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return billing.ErrNotFound
			}
			return fmt.Errorf("billing/postgres: get invoice: %w", err)
		}
		inv = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// ListByTenant returns all invoices for tenantID ordered by period_start DESC.
func (s *Store) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*billing.Invoice, error) {
	if tenantID == uuid.Nil {
		return nil, billing.ErrZeroTenant
	}
	var invs []*billing.Invoice
	err := postgresadapter.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, subscription_id,
			        period_start, period_end,
			        amount_cents_brl, state, cancelled_reason,
			        created_at, updated_at
			   FROM invoice
			  WHERE tenant_id = $1
			  ORDER BY period_start DESC`, tenantID)
		if err != nil {
			return fmt.Errorf("billing/postgres: list invoices: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			inv, err := scanInvoice(rows)
			if err != nil {
				return fmt.Errorf("billing/postgres: scan invoice: %w", err)
			}
			invs = append(invs, inv)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return invs, nil
}

// SaveInvoice inserts or updates the invoice row. The upsert key is the
// invoice's primary key (id); state and cancelled_reason are updated on
// conflict. actorID is recorded in the master_ops audit trail.
func (s *Store) SaveInvoice(ctx context.Context, inv *billing.Invoice, actorID uuid.UUID) error {
	if inv == nil {
		return fmt.Errorf("billing/postgres: invoice is nil")
	}
	return postgresadapter.WithMasterOps(ctx, s.masterPool, actorID, func(tx pgx.Tx) error {
		var cancelledReason any
		if r := inv.CancelledReason(); r != "" {
			cancelledReason = r
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO invoice
			  (id, tenant_id, subscription_id,
			   period_start, period_end,
			   amount_cents_brl, state, cancelled_reason,
			   created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (id) DO UPDATE SET
			  state            = EXCLUDED.state,
			  cancelled_reason = EXCLUDED.cancelled_reason,
			  updated_at       = EXCLUDED.updated_at`,
			inv.ID(), inv.TenantID(), inv.SubscriptionID(),
			inv.PeriodStart(), inv.PeriodEnd(),
			inv.AmountCentsBRL(), string(inv.State()), cancelledReason,
			inv.CreatedAt(), inv.UpdatedAt(),
		)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return billing.ErrInvoiceAlreadyExists
			}
			return fmt.Errorf("billing/postgres: save invoice: %w", err)
		}
		return nil
	})
}

func scanInvoice(row pgx.Row) (*billing.Invoice, error) {
	var (
		id              uuid.UUID
		tenantID        uuid.UUID
		subscriptionID  uuid.UUID
		periodStart     time.Time
		periodEnd       time.Time
		amountCentsBRL  int
		state           string
		cancelledReason *string
		createdAt       time.Time
		updatedAt       time.Time
	)
	if err := row.Scan(
		&id, &tenantID, &subscriptionID,
		&periodStart, &periodEnd,
		&amountCentsBRL, &state, &cancelledReason,
		&createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	var reason string
	if cancelledReason != nil {
		reason = *cancelledReason
	}
	return billing.HydrateInvoice(
		id, tenantID, subscriptionID,
		periodStart, periodEnd,
		amountCentsBRL,
		billing.InvoiceState(state),
		reason,
		createdAt, updatedAt,
	), nil
}
