// Package master is the pgx-backed adapter for the master-side ports
// declared in internal/web/master. This file covers TenantLister,
// TenantCreator, and PlanAssigner via MasterTenantStore.
package master

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
	"github.com/pericles-luz/crm/internal/wallet"
	masterweb "github.com/pericles-luz/crm/internal/web/master"
)

// Compile-time assertions.
var (
	_ masterweb.TenantLister  = (*MasterTenantStore)(nil)
	_ masterweb.TenantCreator = (*MasterTenantStore)(nil)
	_ masterweb.PlanAssigner  = (*MasterTenantStore)(nil)
)

// MasterTenantStore is the pgx-backed adapter for the three tenant-management
// ports. All operations run under WithMasterOps (master_ops pool) because:
//   - List reads across every tenant row (no RLS permitted).
//   - Create/Assign write to tenants / subscription, both audit-trigger gated.
//
// courtesyRepo is optional. When non-nil and CreateTenantInput.InitialCourtesyTokens > 0,
// Issue is called inside the same logical operation (separate tx, atomic at
// the wallet layer). When nil, courtesy bootstrapping is silently skipped.
type MasterTenantStore struct {
	masterOpsPool *pgxpool.Pool
	runtimePool   *pgxpool.Pool // plan reads (no RLS, no WithTenant needed)
	actorID       uuid.UUID
	courtesyRepo  wallet.CourtesyGrantRepository // optional
	now           func() time.Time
}

// TenantStoreOption customises MasterTenantStore at construction time.
type TenantStoreOption func(*MasterTenantStore)

// WithCourtesyRepo attaches the wallet courtesy repository so that
// CreateTenantInput.InitialCourtesyTokens > 0 bootstraps a wallet grant.
func WithCourtesyRepo(r wallet.CourtesyGrantRepository) TenantStoreOption {
	return func(s *MasterTenantStore) { s.courtesyRepo = r }
}

// WithTenantStoreClock overrides the clock for tests.
func WithTenantStoreClock(now func() time.Time) TenantStoreOption {
	return func(s *MasterTenantStore) { s.now = now }
}

// NewMasterTenantStore constructs the store. Both pools must be non-nil;
// masterOpsPool MUST connect as app_master_ops and runtimePool as app_runtime
// (used for plan catalogue reads which have no RLS).
func NewMasterTenantStore(masterOpsPool, runtimePool *pgxpool.Pool, actorID uuid.UUID, opts ...TenantStoreOption) (*MasterTenantStore, error) {
	if masterOpsPool == nil || runtimePool == nil {
		return nil, postgresadapter.ErrNilPool
	}
	if actorID == uuid.Nil {
		return nil, postgresadapter.ErrZeroActor
	}
	s := &MasterTenantStore{
		masterOpsPool: masterOpsPool,
		runtimePool:   runtimePool,
		actorID:       actorID,
		now:           time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// TenantLister
// ---------------------------------------------------------------------------

// List returns a paginated, optionally filtered list of tenant rows.
// It joins tenants → active subscription → plan → latest invoice in a
// single query and computes TotalCount via a window function.
// All reads go through WithMasterOps (cross-tenant, no RLS).
func (s *MasterTenantStore) List(ctx context.Context, opts masterweb.ListOptions) (masterweb.ListResult, error) {
	if opts.PageSize <= 0 {
		opts.PageSize = 25
	}
	if opts.Page <= 0 {
		opts.Page = 1
	}
	offset := (opts.Page - 1) * opts.PageSize

	const q = `
	WITH ranked AS (
	  SELECT t.id, t.name, t.host,
	         COALESCE(p.slug, '')        AS plan_slug,
	         COALESCE(p.name, '')        AS plan_name,
	         COALESCE(s.status, '')      AS sub_status,
	         COALESCE(i.state, '')       AS inv_state,
	         COALESCE(i.updated_at, $3::timestamptz) AS inv_updated_at,
	         COUNT(*) OVER ()            AS total_count
	    FROM tenants t
	    LEFT JOIN subscription s ON s.tenant_id = t.id AND s.status = 'active'
	    LEFT JOIN plan p ON p.id = s.plan_id
	    LEFT JOIN LATERAL (
	      SELECT state, updated_at FROM invoice
	       WHERE tenant_id = t.id
	       ORDER BY updated_at DESC
	       LIMIT 1
	    ) i ON true
	   WHERE ($1::text = '' OR p.slug = $1)
	   ORDER BY t.created_at DESC
	)
	SELECT id, name, host, plan_slug, plan_name, sub_status,
	       inv_state, inv_updated_at, total_count
	  FROM ranked
	 LIMIT $2 OFFSET $4`

	var rows []masterweb.TenantRow
	var totalCount int

	epoch := time.Unix(0, 0).UTC()
	err := postgresadapter.WithMasterOps(ctx, s.masterOpsPool, s.actorID, func(tx pgx.Tx) error {
		pgRows, err := tx.Query(ctx, q,
			opts.FilterPlanSlug, opts.PageSize, epoch, offset,
		)
		if err != nil {
			return fmt.Errorf("master/postgres: list tenants query: %w", err)
		}
		defer pgRows.Close()
		for pgRows.Next() {
			var row masterweb.TenantRow
			var tc int
			if err := pgRows.Scan(
				&row.ID, &row.Name, &row.Host,
				&row.PlanSlug, &row.PlanName, &row.SubscriptionStatus,
				&row.LastInvoiceState, &row.LastInvoiceUpdatedAt, &tc,
			); err != nil {
				return fmt.Errorf("master/postgres: scan tenant row: %w", err)
			}
			rows = append(rows, row)
			totalCount = tc
		}
		return pgRows.Err()
	})
	if err != nil {
		return masterweb.ListResult{}, err
	}

	if rows == nil {
		rows = []masterweb.TenantRow{}
	}
	return masterweb.ListResult{
		Tenants:    rows,
		Page:       opts.Page,
		PageSize:   opts.PageSize,
		TotalCount: totalCount,
	}, nil
}

// ---------------------------------------------------------------------------
// TenantCreator
// ---------------------------------------------------------------------------

// Create inserts a new tenant, optionally assigns an initial plan (subscription),
// and optionally bootstraps a courtesy wallet grant. On host collision it
// returns ErrHostTaken; on unknown plan slug it returns ErrUnknownPlan.
func (s *MasterTenantStore) Create(ctx context.Context, in masterweb.CreateTenantInput) (masterweb.CreateTenantResult, error) {
	now := s.now()
	tenantID := uuid.New()

	var plan billing.Plan
	if in.PlanSlug != "" {
		p, err := s.lookupPlanBySlug(ctx, in.PlanSlug)
		if err != nil {
			return masterweb.CreateTenantResult{}, err
		}
		plan = p
	}

	err := postgresadapter.WithMasterOps(ctx, s.masterOpsPool, s.actorID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, name, host, created_at) VALUES ($1,$2,$3,$4)`,
			tenantID, in.Name, in.Host, now,
		)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return fmt.Errorf("%w: host %q", masterweb.ErrHostTaken, in.Host)
			}
			return fmt.Errorf("master/postgres: insert tenant: %w", err)
		}

		if in.PlanSlug != "" {
			periodStart := now
			periodEnd := now.AddDate(0, 1, 0)
			sub, err := billing.NewSubscription(tenantID, plan.ID, periodStart, periodEnd, now)
			if err != nil {
				return fmt.Errorf("master/postgres: new subscription: %w", err)
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO subscription
				  (id, tenant_id, plan_id, status,
				   current_period_start, current_period_end,
				   created_at, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				sub.ID(), sub.TenantID(), sub.PlanID(), string(sub.Status()),
				sub.CurrentPeriodStart(), sub.CurrentPeriodEnd(),
				sub.CreatedAt(), sub.UpdatedAt(),
			)
			if err != nil {
				return fmt.Errorf("master/postgres: insert subscription: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return masterweb.CreateTenantResult{}, err
	}

	// Courtesy grant runs in its own tx via the wallet adapter.
	if in.InitialCourtesyTokens > 0 && s.courtesyRepo != nil {
		if _, err := s.courtesyRepo.Issue(ctx, tenantID, in.ActorUserID, in.InitialCourtesyTokens); err != nil &&
			!errors.Is(err, wallet.ErrCourtesyGrantDisabled) {
			return masterweb.CreateTenantResult{}, fmt.Errorf("master/postgres: courtesy grant: %w", err)
		}
	}

	row, err := s.loadTenantRow(ctx, tenantID)
	if err != nil {
		return masterweb.CreateTenantResult{}, err
	}
	return masterweb.CreateTenantResult{Tenant: row}, nil
}

// ---------------------------------------------------------------------------
// PlanAssigner
// ---------------------------------------------------------------------------

// Assign creates or transitions the tenant's active subscription to the given
// plan. It cancels the existing active subscription (if any) and inserts a
// new active one. ErrNotFound if tenant missing; ErrUnknownPlan if slug bad.
func (s *MasterTenantStore) Assign(ctx context.Context, in masterweb.AssignPlanInput) (masterweb.AssignPlanResult, error) {
	now := s.now()

	if err := s.assertTenantExists(ctx, in.TenantID); err != nil {
		return masterweb.AssignPlanResult{}, err
	}

	plan, err := s.lookupPlanBySlug(ctx, in.PlanSlug)
	if err != nil {
		return masterweb.AssignPlanResult{}, err
	}

	err = postgresadapter.WithMasterOps(ctx, s.masterOpsPool, s.actorID, func(tx pgx.Tx) error {
		// Cancel existing active subscription if any.
		_, err := tx.Exec(ctx, `
			UPDATE subscription SET status='cancelled', updated_at=$2
			 WHERE tenant_id=$1 AND status='active'`,
			in.TenantID, now,
		)
		if err != nil {
			return fmt.Errorf("master/postgres: cancel subscription: %w", err)
		}

		periodStart := now
		periodEnd := now.AddDate(0, 1, 0)
		sub, err := billing.NewSubscription(in.TenantID, plan.ID, periodStart, periodEnd, now)
		if err != nil {
			return fmt.Errorf("master/postgres: new subscription: %w", err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO subscription
			  (id, tenant_id, plan_id, status,
			   current_period_start, current_period_end,
			   created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			sub.ID(), sub.TenantID(), sub.PlanID(), string(sub.Status()),
			sub.CurrentPeriodStart(), sub.CurrentPeriodEnd(),
			sub.CreatedAt(), sub.UpdatedAt(),
		)
		if err != nil {
			return fmt.Errorf("master/postgres: insert subscription: %w", err)
		}
		return nil
	})
	if err != nil {
		return masterweb.AssignPlanResult{}, err
	}

	row, err := s.loadTenantRow(ctx, in.TenantID)
	if err != nil {
		return masterweb.AssignPlanResult{}, err
	}
	return masterweb.AssignPlanResult{Tenant: row}, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (s *MasterTenantStore) lookupPlanBySlug(ctx context.Context, slug string) (billing.Plan, error) {
	var p billing.Plan
	// plan has no RLS — use the runtime pool for a simple read.
	err := s.runtimePool.QueryRow(ctx,
		`SELECT id, slug, name, price_cents_brl, monthly_token_quota, created_at, updated_at
		   FROM plan WHERE slug = $1`, slug,
	).Scan(&p.ID, &p.Slug, &p.Name, &p.PriceCentsBRL, &p.MonthlyTokenQuota, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return billing.Plan{}, fmt.Errorf("%w: %q", masterweb.ErrUnknownPlan, slug)
		}
		return billing.Plan{}, fmt.Errorf("master/postgres: lookup plan: %w", err)
	}
	return p, nil
}

func (s *MasterTenantStore) assertTenantExists(ctx context.Context, tenantID uuid.UUID) error {
	var exists bool
	err := postgresadapter.WithMasterOps(ctx, s.masterOpsPool, s.actorID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM tenants WHERE id=$1)`, tenantID,
		).Scan(&exists)
	})
	if err != nil {
		return fmt.Errorf("master/postgres: check tenant: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: %s", masterweb.ErrNotFound, tenantID)
	}
	return nil
}

func (s *MasterTenantStore) loadTenantRow(ctx context.Context, tenantID uuid.UUID) (masterweb.TenantRow, error) {
	epoch := time.Unix(0, 0).UTC()
	var row masterweb.TenantRow

	err := postgresadapter.WithMasterOps(ctx, s.masterOpsPool, s.actorID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT t.id, t.name, t.host,
			       COALESCE(p.slug, '')        AS plan_slug,
			       COALESCE(p.name, '')        AS plan_name,
			       COALESCE(s.status, '')      AS sub_status,
			       COALESCE(i.state, '')       AS inv_state,
			       COALESCE(i.updated_at, $2::timestamptz) AS inv_updated_at
			  FROM tenants t
			  LEFT JOIN subscription s ON s.tenant_id = t.id AND s.status = 'active'
			  LEFT JOIN plan p ON p.id = s.plan_id
			  LEFT JOIN LATERAL (
			    SELECT state, updated_at FROM invoice
			     WHERE tenant_id = t.id
			     ORDER BY updated_at DESC
			     LIMIT 1
			  ) i ON true
			 WHERE t.id = $1`,
			tenantID, epoch,
		).Scan(
			&row.ID, &row.Name, &row.Host,
			&row.PlanSlug, &row.PlanName, &row.SubscriptionStatus,
			&row.LastInvoiceState, &row.LastInvoiceUpdatedAt,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return masterweb.TenantRow{}, fmt.Errorf("%w: %s", masterweb.ErrNotFound, tenantID)
		}
		return masterweb.TenantRow{}, fmt.Errorf("master/postgres: load tenant row: %w", err)
	}
	return row, nil
}
