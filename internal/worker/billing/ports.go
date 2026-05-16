// Package billing hosts the BillingRenewer worker for SIN-62879 / Fase 2.5
// C5. The worker sweeps active subscriptions whose current_period_end has
// elapsed, atomically advances them by one month, inserts a pending invoice
// for the new period, and publishes a subscription.renewed event on
// JetStream with Nats-Msg-Id = "{subscription_id}:{new_period_start_iso}"
// so a redelivery within the F-14 1h window is dropped by JetStream dedup.
//
// Idempotency is enforced end-to-end:
//
//   - The invoice partial UNIQUE index (tenant_id, period_start) WHERE
//     state <> 'cancelled_by_master' rejects the second insert for the
//     same period, surfaced as billing.ErrInvoiceAlreadyExists. The
//     renewer counts that outcome as skipped_already_done.
//   - The subscription period advance and the invoice insert run inside
//     a single master_ops transaction; a partial run can never leak a
//     period mismatch.
//   - The NATS dedup key is deterministic — the same advancement always
//     produces the same msg-id, so re-publishing after a crash dedups
//     server-side.
//
// Domain code in this package MUST stay free of database/sql, pgx, and
// NATS SDK imports. Storage lives behind DueSubscriptionsLister and
// SubscriptionRenewer; the JetStream publish lives behind EventPublisher;
// the Postgres adapter in internal/adapter/db/postgres/billing is the
// only blessed implementation.
package billing

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing"
)

// DueSubscription is the projection the renewer needs per subscription:
// identity, owning tenant, plan price (joined in to avoid an extra
// round-trip), and the period boundary that triggered the renewal.
type DueSubscription struct {
	ID               uuid.UUID
	TenantID         uuid.UUID
	PlanID           uuid.UUID
	PlanPriceCents   int
	CurrentPeriodEnd time.Time
}

// DueSubscriptionsLister returns active subscriptions whose
// current_period_end is at or before asOf. Implementations MUST return
// rows ordered by current_period_end ascending so the oldest is renewed
// first; limit bounds the per-tick batch so a backlog cannot starve
// graceful shutdown.
//
// The query crosses tenants and therefore requires the master_ops role.
// Implementations SHOULD scan small batches (default 100) and let the
// next tick pick up the remainder.
type DueSubscriptionsLister interface {
	ListDueSubscriptions(ctx context.Context, asOf time.Time, limit int) ([]DueSubscription, error)
}

// RenewResult bundles the post-renewal state of one subscription so the
// renewer can build the event payload without a second round-trip.
type RenewResult struct {
	Invoice        *billing.Invoice
	Subscription   *billing.Subscription
	NewPeriodStart time.Time
	NewPeriodEnd   time.Time
}

// SubscriptionRenewer is the write port. RenewSubscription atomically:
//
//   - inserts a pending invoice for the new period
//     [oldPeriodEnd, oldPeriodEnd + 1 month) at planPriceCents;
//   - advances the subscription row to the same period boundaries.
//
// Both writes happen inside one master_ops transaction so the audit
// trigger records the actor. Implementations MUST translate the
// (tenant_id, period_start) partial UNIQUE violation to
// billing.ErrInvoiceAlreadyExists. They MUST also return that sentinel
// when the optimistic lock on subscription.current_period_end fails —
// i.e. the period was advanced by a concurrent worker between the
// listing and the write.
type SubscriptionRenewer interface {
	RenewSubscription(
		ctx context.Context,
		subID uuid.UUID,
		oldPeriodEnd time.Time,
		planPriceCents int,
		actorID uuid.UUID,
		now time.Time,
	) (RenewResult, error)
}

// EventPublisher is the narrow JetStream surface the renewer needs. The
// real adapter wraps internal/adapter/messaging/nats; tests pass a
// closure-backed fake. msgID is the JetStream Nats-Msg-Id header used
// for cross-publication dedup inside the stream Duplicates window.
type EventPublisher interface {
	Publish(ctx context.Context, subject string, msgID string, body []byte) error
}
