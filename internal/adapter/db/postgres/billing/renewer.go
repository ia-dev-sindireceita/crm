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
	billingworker "github.com/pericles-luz/crm/internal/worker/billing"
)

// RenewerStore implements both billingworker.DueSubscriptionsLister and
// billingworker.SubscriptionRenewer against Postgres. All queries route
// through the app_master_ops role because the renewer reads across
// tenants and writes audited rows (subscription + invoice). RLS does
// not apply (BYPASSRLS=true) and the master_ops_audit trigger records
// the actorID.
type RenewerStore struct {
	masterPool *pgxpool.Pool
}

// NewRenewerStore constructs a RenewerStore. masterPool MUST be the
// app_master_ops pool — using app_runtime here would either silently
// hide active subscriptions in other tenants (RLS gate) or trip the
// audit trigger.
func NewRenewerStore(masterPool *pgxpool.Pool) (*RenewerStore, error) {
	if masterPool == nil {
		return nil, postgresadapter.ErrNilPool
	}
	return &RenewerStore{masterPool: masterPool}, nil
}

// Compile-time assertions.
var (
	_ billingworker.DueSubscriptionsLister = (*RenewerStore)(nil)
	_ billingworker.SubscriptionRenewer    = (*RenewerStore)(nil)
)

// ListDueSubscriptions returns active subscriptions whose
// current_period_end <= asOf, joined with their plan to surface the
// price. The query reads from the master role because it crosses
// tenants; RLS is not in play here.
func (s *RenewerStore) ListDueSubscriptions(ctx context.Context, asOf time.Time, limit int) ([]billingworker.DueSubscription, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.masterPool.Query(ctx, `
		SELECT s.id, s.tenant_id, s.plan_id, p.price_cents_brl, s.current_period_end
		  FROM subscription s
		  JOIN plan p ON p.id = s.plan_id
		 WHERE s.status = 'active'
		   AND s.current_period_end <= $1
		 ORDER BY s.current_period_end ASC
		 LIMIT $2`, asOf, limit)
	if err != nil {
		return nil, fmt.Errorf("billing/postgres: list due subscriptions: %w", err)
	}
	defer rows.Close()
	var out []billingworker.DueSubscription
	for rows.Next() {
		var d billingworker.DueSubscription
		if err := rows.Scan(&d.ID, &d.TenantID, &d.PlanID, &d.PlanPriceCents, &d.CurrentPeriodEnd); err != nil {
			return nil, fmt.Errorf("billing/postgres: scan due subscription: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("billing/postgres: list due subscriptions: %w", err)
	}
	return out, nil
}

// RenewSubscription atomically advances subID's period by one month
// (new boundaries: [oldPeriodEnd, oldPeriodEnd + 1 month]) and inserts
// a pending invoice for the new period at planPriceCents. The pair
// runs inside a single WithMasterOps transaction so the audit trigger
// fires and a partial run never leaks.
//
// The insert is ordered FIRST so the partial UNIQUE index
// (tenant_id, period_start) WHERE state <> 'cancelled_by_master' is
// the gating idempotency check; the subscription update only runs
// when the invoice landed for this exact period.
//
// On a duplicate insert, RenewSubscription returns
// billing.ErrInvoiceAlreadyExists so the worker can count
// skipped_already_done and move on. On the optimistic-lock failure
// (a concurrent worker advanced this subscription between listing
// and write) the same sentinel is returned, for the same reason: the
// invoice for THIS period already exists and our retry must skip.
func (s *RenewerStore) RenewSubscription(
	ctx context.Context,
	subID uuid.UUID,
	oldPeriodEnd time.Time,
	planPriceCents int,
	actorID uuid.UUID,
	now time.Time,
) (billingworker.RenewResult, error) {
	if subID == uuid.Nil {
		return billingworker.RenewResult{}, fmt.Errorf("billing/postgres: subID is required")
	}
	newPeriodStart := oldPeriodEnd
	newPeriodEnd := oldPeriodEnd.AddDate(0, 1, 0)

	var (
		invoiceID uuid.UUID
		tenantID  uuid.UUID
		planID    uuid.UUID
	)
	err := postgresadapter.WithMasterOps(ctx, s.masterPool, actorID, func(tx pgx.Tx) error {
		// Resolve tenant + plan from the subscription row. The query
		// also serves as the existence check; if the subscription is
		// missing we propagate ErrNotFound so the worker logs and
		// skips. Locking is not strictly required — the partial UNIQUE
		// on invoice gives us the idempotency floor — but we still
		// SELECT FOR UPDATE so a concurrent UPDATE on the same row
		// serializes behind us and the optimistic check below holds.
		if err := tx.QueryRow(ctx, `
			SELECT tenant_id, plan_id
			  FROM subscription
			 WHERE id = $1 AND status = 'active'
			 FOR UPDATE`, subID,
		).Scan(&tenantID, &planID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return billing.ErrNotFound
			}
			return fmt.Errorf("billing/postgres: lock subscription: %w", err)
		}

		invoiceID = uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO invoice
			  (id, tenant_id, subscription_id,
			   period_start, period_end,
			   amount_cents_brl, state,
			   created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $7)`,
			invoiceID, tenantID, subID,
			newPeriodStart, newPeriodEnd,
			planPriceCents, now,
		); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return billing.ErrInvoiceAlreadyExists
			}
			return fmt.Errorf("billing/postgres: insert invoice: %w", err)
		}

		// Optimistic advance: the WHERE clause guards against a
		// concurrent worker who already advanced this subscription.
		// If 0 rows were affected, the row was already moved past
		// oldPeriodEnd and our invoice insert "won" only because the
		// other worker happened to use a different period; treat this
		// as the duplicate path.
		cmd, err := tx.Exec(ctx, `
			UPDATE subscription
			   SET current_period_start = $1,
			       current_period_end   = $2,
			       updated_at           = $3
			 WHERE id = $4
			   AND current_period_end = $5`,
			newPeriodStart, newPeriodEnd, now, subID, oldPeriodEnd,
		)
		if err != nil {
			return fmt.Errorf("billing/postgres: advance subscription: %w", err)
		}
		if cmd.RowsAffected() == 0 {
			return billing.ErrInvoiceAlreadyExists
		}
		return nil
	})
	if err != nil {
		return billingworker.RenewResult{}, err
	}

	sub := billing.HydrateSubscription(
		subID, tenantID, planID,
		billing.SubscriptionStatusActive,
		newPeriodStart, newPeriodEnd,
		now, now,
	)
	inv := billing.HydrateInvoice(
		invoiceID, tenantID, subID,
		newPeriodStart, newPeriodEnd,
		planPriceCents,
		billing.InvoiceStatePending,
		"",
		now, now,
	)
	return billingworker.RenewResult{
		Invoice:        inv,
		Subscription:   sub,
		NewPeriodStart: newPeriodStart,
		NewPeriodEnd:   newPeriodEnd,
	}, nil
}
