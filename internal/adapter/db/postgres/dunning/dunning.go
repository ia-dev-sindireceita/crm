// Package dunning (adapter) implements the dunning persistence and
// candidate-listing ports against Postgres.
//
// Three surfaces:
//
//   - Store implements [billingdunning.DunningRepository] for the
//     per-subscription GetBySubscription/Save round-trip used by the
//     C13 webhook handler and by tenant-side reads (CurrentForTenant).
//   - TickStore implements the worker-side
//     [workerdunning.CandidatesLister] + [workerdunning.Saver] needed
//     to drive the tick.
//   - CourtesyOverrideStore implements
//     [billingdunning.CourtesyOverride] against master_grant.
//
// Read paths under WithTenant use the runtime pool (RLS gates by
// tenant). Cross-tenant scans (the tick listing) and all writes use
// the master_ops pool (BYPASSRLS=true + audit trigger).
package dunning

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	billingdunning "github.com/pericles-luz/crm/internal/billing/dunning"
	workerdunning "github.com/pericles-luz/crm/internal/worker/dunning"
)

// Store implements billingdunning.DunningRepository. Reads route through
// runtimePool under WithTenant; writes route through masterPool under
// WithMasterOps.
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

var _ billingdunning.DunningRepository = (*Store)(nil)

// GetBySubscription returns the dunning row for subscriptionID. Reads
// happen under master_ops because the webhook handler (the primary
// caller) does not have a tenant context — it works from the invoice
// payload. This is a controlled BYPASSRLS path; the call only ever
// fetches a single row by its subscription_id PK.
func (s *Store) GetBySubscription(ctx context.Context, subscriptionID uuid.UUID) (*billingdunning.DunningState, error) {
	if subscriptionID == uuid.Nil {
		return nil, billingdunning.ErrZeroSubscription
	}
	row, err := scanDunningState(s.masterPool.QueryRow(ctx,
		`SELECT id, tenant_id, subscription_id, state, entered_state_at,
		        COALESCE(last_invoice_id, '00000000-0000-0000-0000-000000000000'::uuid),
		        override_until, override_reason
		   FROM subscription_dunning_states
		  WHERE subscription_id = $1`, subscriptionID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, billingdunning.ErrNotFound
		}
		return nil, fmt.Errorf("dunning/postgres: get by subscription: %w", err)
	}
	return row, nil
}

// Save upserts the dunning row on subscription_id. actorID is recorded
// by the master_ops audit trigger.
func (s *Store) Save(ctx context.Context, d *billingdunning.DunningState, actorID uuid.UUID) error {
	if d == nil {
		return fmt.Errorf("dunning/postgres: nil dunning state")
	}
	return postgresadapter.WithMasterOps(ctx, s.masterPool, actorID, func(tx pgx.Tx) error {
		var lastInvoice any
		if id := d.LastInvoiceID(); id != uuid.Nil {
			lastInvoice = id
		}
		var until any
		if t := d.OverrideUntil(); t != nil {
			until = *t
		}
		var reason any
		if r := d.OverrideReason(); r != "" {
			reason = r
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO subscription_dunning_states
			  (id, tenant_id, subscription_id, state, entered_state_at,
			   last_invoice_id, override_until, override_reason)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (subscription_id) DO UPDATE SET
			   state            = EXCLUDED.state,
			   entered_state_at = EXCLUDED.entered_state_at,
			   last_invoice_id  = EXCLUDED.last_invoice_id,
			   override_until   = EXCLUDED.override_until,
			   override_reason  = EXCLUDED.override_reason`,
			d.ID(), d.TenantID(), d.SubscriptionID(), string(d.State()), d.EnteredStateAt(),
			lastInvoice, until, reason,
		)
		if err != nil {
			return fmt.Errorf("dunning/postgres: save: %w", err)
		}
		return nil
	})
}

func scanDunningState(row pgx.Row) (*billingdunning.DunningState, error) {
	var (
		id, tenantID, subID, lastInvoice uuid.UUID
		state                            string
		enteredAt                        time.Time
		overrideUntil                    *time.Time
		overrideReason                   *string
	)
	if err := row.Scan(&id, &tenantID, &subID, &state, &enteredAt, &lastInvoice,
		&overrideUntil, &overrideReason); err != nil {
		return nil, err
	}
	var reason string
	if overrideReason != nil {
		reason = *overrideReason
	}
	return billingdunning.HydrateDunningState(
		id, tenantID, subID,
		billingdunning.State(state),
		enteredAt, lastInvoice,
		overrideUntil, reason,
	), nil
}

// CurrentForTenant returns the tenant's active subscription's dunning
// row, or billingdunning.ErrNotFound. Runs under WithTenant so RLS
// gates visibility to the calling tenant — defense in depth.
//
// Used by the web/billing/invoices handler to render the dunning banner.
func (s *Store) CurrentForTenant(ctx context.Context, tenantID uuid.UUID) (*billingdunning.DunningState, error) {
	if tenantID == uuid.Nil {
		return nil, billingdunning.ErrZeroTenant
	}
	var got *billingdunning.DunningState
	err := postgresadapter.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		row, err := scanDunningState(tx.QueryRow(ctx,
			`SELECT sds.id, sds.tenant_id, sds.subscription_id, sds.state, sds.entered_state_at,
			        COALESCE(sds.last_invoice_id, '00000000-0000-0000-0000-000000000000'::uuid),
			        sds.override_until, sds.override_reason
			   FROM subscription_dunning_states sds
			   JOIN subscription s ON s.id = sds.subscription_id AND s.status = 'active'
			  WHERE sds.tenant_id = $1`, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return billingdunning.ErrNotFound
			}
			return fmt.Errorf("dunning/postgres: current for tenant: %w", err)
		}
		got = row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return got, nil
}

// ---------------------------------------------------------------------------
// TickStore — worker-side projection.
// ---------------------------------------------------------------------------

// TickStore implements workerdunning.CandidatesLister + Saver against
// Postgres. The listing is a single LEFT JOIN LATERAL query that joins
// subscription_dunning_states with its active subscription and the
// OLDEST pending invoice (period_start ASC); a row with no pending
// invoice surfaces with a nil Pending. The save delegates to the
// wrapped Store.
type TickStore struct {
	master *pgxpool.Pool
	store  *Store
}

// NewTickStore constructs a TickStore. masterPool MUST be the
// app_master_ops pool — the listing is cross-tenant.
func NewTickStore(store *Store, masterPool *pgxpool.Pool) (*TickStore, error) {
	if store == nil || masterPool == nil {
		return nil, postgresadapter.ErrNilPool
	}
	return &TickStore{master: masterPool, store: store}, nil
}

var (
	_ workerdunning.CandidatesLister = (*TickStore)(nil)
	_ workerdunning.Saver            = (*TickStore)(nil)
)

// ListCandidates returns the non-terminal dunning rows the worker
// should evaluate this tick. Implementations of CandidatesLister are
// allowed to ignore asOf; we do, because the rows are state-driven
// (the tick re-evaluates every non-cancelled row every cadence).
//
// The query LEFT JOINs the oldest pending invoice; subscriptions with
// no pending invoice surface with Pending == nil so the worker can
// idempotently MarkPaid.
func (s *TickStore) ListCandidates(ctx context.Context, _ time.Time, limit int) ([]workerdunning.Candidate, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.master.Query(ctx, `
		SELECT sds.id, sds.tenant_id, sds.subscription_id, sds.state, sds.entered_state_at,
		       COALESCE(sds.last_invoice_id, '00000000-0000-0000-0000-000000000000'::uuid),
		       sds.override_until, sds.override_reason,
		       s.plan_id,
		       inv.id, inv.period_start
		  FROM subscription_dunning_states sds
		  JOIN subscription s ON s.id = sds.subscription_id AND s.status = 'active'
		  LEFT JOIN LATERAL (
		    SELECT id, period_start
		      FROM invoice
		     WHERE subscription_id = sds.subscription_id AND state = 'pending'
		     ORDER BY period_start ASC
		     LIMIT 1
		  ) inv ON TRUE
		 WHERE sds.state IN ('current','warn','suspended_outbound','suspended_full')
		 ORDER BY sds.entered_state_at ASC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("dunning/postgres: list candidates: %w", err)
	}
	defer rows.Close()

	var out []workerdunning.Candidate
	for rows.Next() {
		var (
			id, tenantID, subID, lastInvoice, planID uuid.UUID
			state                                    string
			enteredAt                                time.Time
			overrideUntil                            *time.Time
			overrideReason                           *string
			pendingID                                *uuid.UUID
			pendingPeriodStart                       *time.Time
		)
		if err := rows.Scan(&id, &tenantID, &subID, &state, &enteredAt, &lastInvoice,
			&overrideUntil, &overrideReason, &planID,
			&pendingID, &pendingPeriodStart); err != nil {
			return nil, fmt.Errorf("dunning/postgres: scan candidate: %w", err)
		}
		var reason string
		if overrideReason != nil {
			reason = *overrideReason
		}
		row := billingdunning.HydrateDunningState(
			id, tenantID, subID,
			billingdunning.State(state),
			enteredAt, lastInvoice,
			overrideUntil, reason,
		)
		c := workerdunning.Candidate{
			Row:            row,
			SubscriptionID: subID,
			TenantID:       tenantID,
			PlanID:         planID,
		}
		if pendingID != nil && pendingPeriodStart != nil {
			c.Pending = &workerdunning.PendingInvoice{
				ID:          *pendingID,
				PeriodStart: *pendingPeriodStart,
			}
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dunning/postgres: list candidates: %w", err)
	}
	return out, nil
}

// Save delegates to the underlying Store so the upsert SQL lives in
// exactly one place.
func (s *TickStore) Save(ctx context.Context, d *billingdunning.DunningState, actorID uuid.UUID) error {
	return s.store.Save(ctx, d, actorID)
}
