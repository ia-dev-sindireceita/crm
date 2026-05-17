// Package masterview implements the read-only ports backing the master
// operator's C11 billing-history + token-ledger views
// (web/master.BillingViewer and web/master.LedgerViewer).
//
// Both adapters route through WithTenant against the runtime pool so
// RLS scopes the visible rows to the requested tenant id; that is the
// SQL-side guarantee for AC #3 (gerente of tenant Y NÃO vê tenant X).
// The handler layer adds a defensive cross-tenant gate above this
// adapter — see web/master.crossTenantPermitted.
package masterview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/web/master"
)

var (
	_ master.BillingViewer = (*Store)(nil)
	_ master.LedgerViewer  = (*Store)(nil)
)

// Store implements the two C11 ports against Postgres.
//
// runtimePool connects as app_runtime (RLS enforced). Every read
// goes through WithTenant so the policy gates visibility — a gerente
// of tenant Y fetching tenant X's URL gets zero rows because the
// tenant_isolation_select RLS policy compares app.tenant_id (set by
// WithTenant) to the row's tenant_id.
type Store struct {
	runtimePool *pgxpool.Pool
}

// New constructs a Store. nil pool is rejected so the wire fails fast.
func New(runtimePool *pgxpool.Pool) (*Store, error) {
	if runtimePool == nil {
		return nil, postgresadapter.ErrNilPool
	}
	return &Store{runtimePool: runtimePool}, nil
}

// ViewBilling returns the BillingView aggregate for tenantID. The
// transaction issues four sequential SELECTs (subscription+plan,
// invoices, grants) so a single WithTenant scope covers them all and
// the reads see a consistent snapshot.
func (s *Store) ViewBilling(ctx context.Context, tenantID uuid.UUID) (master.BillingView, error) {
	if tenantID == uuid.Nil {
		return master.BillingView{}, master.ErrTenantNotFound
	}
	view := master.BillingView{TenantID: tenantID}
	err := postgresadapter.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		sub, err := scanSubscriptionRow(tx, ctx, tenantID)
		if err != nil {
			return err
		}
		view.Subscription = sub
		invs, err := scanInvoices(tx, ctx, tenantID)
		if err != nil {
			return err
		}
		view.Invoices = invs
		grants, err := scanGrants(tx, ctx, tenantID)
		if err != nil {
			return err
		}
		view.Grants = grants
		return nil
	})
	if err != nil {
		return master.BillingView{}, err
	}
	return view, nil
}

// ViewLedger returns the cursor-paginated token ledger for tenantID.
// The query fetches one extra row beyond opts.PageSize so HasMore is
// known without a second round trip — the extra row is dropped from
// the returned Entries and its (occurred_at,id) becomes the cursor.
func (s *Store) ViewLedger(ctx context.Context, opts master.LedgerOptions) (master.LedgerPage, error) {
	if opts.TenantID == uuid.Nil {
		return master.LedgerPage{}, master.ErrTenantNotFound
	}
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}
	var rows []master.LedgerRow
	err := postgresadapter.WithTenant(ctx, s.runtimePool, opts.TenantID, func(tx pgx.Tx) error {
		got, err := scanLedger(tx, ctx, opts, pageSize+1)
		if err != nil {
			return err
		}
		rows = got
		return nil
	})
	if err != nil {
		return master.LedgerPage{}, err
	}
	page := master.LedgerPage{}
	if len(rows) > pageSize {
		page.Entries = rows[:pageSize]
		page.HasMore = true
		last := page.Entries[len(page.Entries)-1]
		page.NextCursorOccurredAt = last.OccurredAt
		page.NextCursorID = last.ID
	} else {
		page.Entries = rows
	}
	return page, nil
}

// --- subscription + plan ---------------------------------------------------

const selectSubscriptionWithPlan = `
	SELECT s.id, s.plan_id, p.slug, p.name, p.price_cents_brl,
	       s.status, s.current_period_start, s.current_period_end
	  FROM subscription s
	  JOIN plan p ON p.id = s.plan_id
	 WHERE s.tenant_id = $1 AND s.status = 'active'
	 LIMIT 1
`

func scanSubscriptionRow(tx pgx.Tx, ctx context.Context, tenantID uuid.UUID) (master.SubscriptionRow, error) {
	var (
		id, planID             uuid.UUID
		slug, name, status     string
		price                  int
		periodStart, periodEnd time.Time
	)
	err := tx.QueryRow(ctx, selectSubscriptionWithPlan, tenantID).Scan(
		&id, &planID, &slug, &name, &price, &status, &periodStart, &periodEnd,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No active subscription is a legitimate empty state — the
			// view degrades gracefully rather than 404-ing.
			return master.SubscriptionRow{}, nil
		}
		return master.SubscriptionRow{}, fmt.Errorf("masterview: scan subscription: %w", err)
	}
	return master.SubscriptionRow{
		ID:                 id,
		PlanID:             planID,
		PlanSlug:           slug,
		PlanName:           name,
		PlanPriceCentsBRL:  price,
		Status:             status,
		CurrentPeriodStart: periodStart,
		CurrentPeriodEnd:   periodEnd,
		NextInvoiceAt:      periodEnd,
	}, nil
}

// --- invoices --------------------------------------------------------------

const selectInvoicesByTenant = `
	SELECT id, period_start, period_end, amount_cents_brl, state
	  FROM invoice
	 WHERE tenant_id = $1
	 ORDER BY period_start DESC
`

func scanInvoices(tx pgx.Tx, ctx context.Context, tenantID uuid.UUID) ([]master.InvoiceRow, error) {
	rows, err := tx.Query(ctx, selectInvoicesByTenant, tenantID)
	if err != nil {
		return nil, fmt.Errorf("masterview: query invoices: %w", err)
	}
	defer rows.Close()
	out := make([]master.InvoiceRow, 0)
	for rows.Next() {
		var (
			id                     uuid.UUID
			periodStart, periodEnd time.Time
			amount                 int
			state                  string
		)
		if err := rows.Scan(&id, &periodStart, &periodEnd, &amount, &state); err != nil {
			return nil, fmt.Errorf("masterview: scan invoice: %w", err)
		}
		out = append(out, master.InvoiceRow{
			ID:             id,
			PeriodStart:    periodStart,
			PeriodEnd:      periodEnd,
			AmountCentsBRL: amount,
			State:          state,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("masterview: iterate invoices: %w", err)
	}
	return out, nil
}

// --- grants (AC #1) --------------------------------------------------------

const selectGrantsByTenant = `
	SELECT id, external_id, tenant_id, kind, payload, reason,
	       created_by_user_id, created_at,
	       consumed_at, revoked_at, revoked_by_user_id
	  FROM master_grant
	 WHERE tenant_id = $1
	 ORDER BY created_at DESC
`

func scanGrants(tx pgx.Tx, ctx context.Context, tenantID uuid.UUID) ([]master.GrantRow, error) {
	rows, err := tx.Query(ctx, selectGrantsByTenant, tenantID)
	if err != nil {
		return nil, fmt.Errorf("masterview: query grants: %w", err)
	}
	defer rows.Close()
	out := make([]master.GrantRow, 0)
	for rows.Next() {
		var (
			id, gTenantID, createdByID uuid.UUID
			externalID, kind, reason   string
			payloadRaw                 []byte
			createdAt                  time.Time
			consumedAt, revokedAt      *time.Time
			revokedByID                *uuid.UUID
		)
		if err := rows.Scan(
			&id, &externalID, &gTenantID, &kind, &payloadRaw, &reason,
			&createdByID, &createdAt,
			&consumedAt, &revokedAt, &revokedByID,
		); err != nil {
			return nil, fmt.Errorf("masterview: scan grant: %w", err)
		}
		row := master.GrantRow{
			ID:          id,
			ExternalID:  externalID,
			TenantID:    gTenantID,
			Kind:        master.GrantKind(kind),
			Reason:      reason,
			CreatedByID: createdByID,
			CreatedAt:   createdAt,
		}
		applyGrantPayload(&row, payloadRaw)
		if consumedAt != nil {
			row.Consumed = true
			row.ConsumedAt = *consumedAt
		}
		if revokedAt != nil {
			row.Revoked = true
			row.RevokedAt = *revokedAt
			if revokedByID != nil {
				row.RevokeBy = *revokedByID
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("masterview: iterate grants: %w", err)
	}
	return out, nil
}

// applyGrantPayload decodes the JSON payload column into the row's
// kind-specific fields (Amount for extra_tokens, PeriodDays for
// free_subscription_period). Missing fields collapse to zero — the
// template renders "—" in that case.
//
// The payload column is jsonb NOT NULL with a `'{}'::jsonb` default,
// so the bytes always parse — no need for empty / decode-error
// defenses here.
func applyGrantPayload(row *master.GrantRow, raw []byte) {
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	switch row.Kind {
	case master.GrantKindExtraTokens:
		row.Amount = readInt64(p, "amount")
	case master.GrantKindFreeSubscriptionPeriod:
		row.PeriodDays = int(readInt64(p, "period_days"))
	}
}

// readInt64 reads a numeric key out of a json.Unmarshal-derived
// payload map. json.Unmarshal always decodes numbers into float64, so
// the type assertion below is the only path that needs to survive.
// Missing key or non-numeric value collapses to zero — the template
// renders "—" in that case.
func readInt64(p map[string]any, key string) int64 {
	v, ok := p[key]
	if !ok {
		return 0
	}
	n, ok := v.(float64)
	if !ok {
		return 0
	}
	return int64(n)
}

// --- ledger (AC #2 — cursor) -----------------------------------------------

// The cursor predicate is "rows strictly before (occurred_at, id) DESC":
//
//	occurred_at < $cursor_at
//	OR (occurred_at = $cursor_at AND id < $cursor_id)
//
// The first-page branch ($cursor_at = epoch) lets the planner skip the
// inequality entirely and just LIMIT off the tenant-occurred index in
// descending order.
//
// The query LEFT JOINs subscription (to surface the current subscription
// id + plan slug for monthly_alloc rows that fall inside an active
// period) and master_grant (for the master_grant_id → external_id /
// kind lookup). Both joins are nullable; rows without a match scan as
// NULL and the row builder skips the cross-reference.
const selectLedgerByTenant = `
	SELECT l.id, l.occurred_at, l.created_at,
	       l.source, l.kind, l.amount,
	       l.master_grant_id, mg.external_id,
	       s.id, p.slug,
	       COALESCE(l.external_ref, ''),
	       COALESCE(l.idempotency_key, '')
	  FROM token_ledger l
	  LEFT JOIN master_grant mg ON mg.id = l.master_grant_id
	  LEFT JOIN subscription s
	         ON s.tenant_id = l.tenant_id
	        AND s.status = 'active'
	        AND l.occurred_at >= s.current_period_start
	        AND l.occurred_at <  s.current_period_end
	  LEFT JOIN plan p ON p.id = s.plan_id
	 WHERE l.tenant_id = $1
	   AND ($2::timestamptz IS NULL
	        OR l.occurred_at < $2::timestamptz
	        OR (l.occurred_at = $2::timestamptz AND l.id < $3::uuid))
	 ORDER BY l.occurred_at DESC, l.id DESC
	 LIMIT $4
`

func scanLedger(tx pgx.Tx, ctx context.Context, opts master.LedgerOptions, limit int) ([]master.LedgerRow, error) {
	// nil cursor_at means "first page". A typed nil is cleaner than
	// scattering "use sentinel epoch" magic into the SQL because the
	// IS NULL branch lets the planner skip the inequality check
	// entirely and walk the (tenant_id, occurred_at DESC) index.
	var cursorAt any
	var cursorID any
	if !opts.CursorOccurredAt.IsZero() {
		cursorAt = opts.CursorOccurredAt
		cursorID = opts.CursorID
	}
	rows, err := tx.Query(ctx, selectLedgerByTenant, opts.TenantID, cursorAt, cursorID, limit)
	if err != nil {
		return nil, fmt.Errorf("masterview: query ledger: %w", err)
	}
	defer rows.Close()
	out := make([]master.LedgerRow, 0, limit)
	for rows.Next() {
		var (
			id                          uuid.UUID
			occurredAt, createdAt       time.Time
			source, kind                string
			amount                      int64
			masterGrantID               *uuid.UUID
			masterGrantExternalID       *string
			subscriptionID              *uuid.UUID
			subscriptionPlanSlug        *string
			externalRef, idempotencyKey string
		)
		if err := rows.Scan(
			&id, &occurredAt, &createdAt,
			&source, &kind, &amount,
			&masterGrantID, &masterGrantExternalID,
			&subscriptionID, &subscriptionPlanSlug,
			&externalRef, &idempotencyKey,
		); err != nil {
			return nil, fmt.Errorf("masterview: scan ledger: %w", err)
		}
		row := master.LedgerRow{
			ID:             id,
			OccurredAt:     occurredAt,
			CreatedAt:      createdAt,
			Source:         source,
			Kind:           kind,
			Amount:         amount,
			ExternalRef:    externalRef,
			IdempotencyKey: idempotencyKey,
		}
		if masterGrantID != nil {
			row.MasterGrantID = *masterGrantID
		}
		if masterGrantExternalID != nil {
			row.MasterGrantExternalID = *masterGrantExternalID
		}
		if subscriptionID != nil {
			row.SubscriptionID = *subscriptionID
		}
		if subscriptionPlanSlug != nil {
			row.SubscriptionPlanSlug = *subscriptionPlanSlug
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("masterview: iterate ledger: %w", err)
	}
	return out, nil
}
