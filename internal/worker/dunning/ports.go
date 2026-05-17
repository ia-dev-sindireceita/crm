// Package dunning hosts the dunning Tick worker for SIN-62965 / Fase 4 C14.
//
// The worker drives [dunning.DunningState] forward (and back to current)
// every TickEvery for every non-cancelled subscription. It is a thin
// orchestrator over three ports:
//
//   - CandidatesLister  — paged listing of non-terminal dunning rows
//     joined with the oldest pending invoice and the owning subscription.
//     One row per active subscription with a dunning entry; absence of
//     a pending invoice means "no past-due window" and triggers
//     MarkPaid if the row is not already current.
//   - Saver             — persists DunningState mutations via the
//     billing/dunning DunningRepository port (master_ops + audit).
//   - dunning.CourtesyOverride — read-only lookup of the live
//     free_subscription_period grant for the tenant; the worker forwards
//     a non-nil override to Escalate which pauses the state machine.
//
// The package follows the hexagonal rule: no database/sql, pgx, or HTTP
// imports. Adapters live in
// internal/adapter/db/postgres/dunning and wire the cron at boot via
// cmd/server/dunning_wire.go.
//
// Reference decision: ADR 0100
// ([docs/adr/0100-dunning-state-machine.md](../../../docs/adr/0100-dunning-state-machine.md))
// and board ratification D1 in [SIN-62204](/SIN/issues/SIN-62204).
package dunning

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing/dunning"
)

// PendingInvoice describes the oldest unpaid invoice on a subscription
// at the time of the tick. PeriodStart is treated as the "due date"
// for escalation: the renewer creates each invoice at the start of the
// new period (period_start = previous current_period_end), so an unpaid
// row that has been around for N days has been past-due for N days.
type PendingInvoice struct {
	ID          uuid.UUID
	PeriodStart time.Time
}

// Candidate is the projection the tick worker reads per subscription.
// All fields originate from a single LEFT JOIN LATERAL query against
// subscription_dunning_states, subscription, and invoice — see
// internal/adapter/db/postgres/dunning for the SQL.
//
// PendingInvoice is nil when the subscription has no pending invoice
// (either all paid or none yet issued); the tick treats that as
// "no past-due window" and downgrades to current if the row is above it.
type Candidate struct {
	// Row is the hydrated DunningState aggregate. Mutations applied by
	// the tick mutate this in place; Saver persists it on transition.
	Row *dunning.DunningState

	// SubscriptionID and TenantID denormalised onto the candidate so
	// the worker logs structured fields without dereferencing Row.
	SubscriptionID uuid.UUID
	TenantID       uuid.UUID

	// PlanID is the subscription's plan; the worker uses it to look up
	// the per-plan dunning policy (PolicyResolver). uuid.Nil never
	// appears in production data (FK on subscription.plan_id).
	PlanID uuid.UUID

	// Pending is the oldest pending invoice, or nil if all paid.
	Pending *PendingInvoice
}

// CandidatesLister lists non-terminal dunning rows that the tick needs
// to evaluate. Implementations cross tenants and therefore run under
// master_ops (BYPASSRLS=true). Rows are ordered by entered_state_at
// ascending so the oldest unhandled row gets processed first; limit
// bounds the per-tick batch so a backlog cannot starve graceful
// shutdown.
//
// Implementations MUST NOT return rows in StateCancelled — the worker
// treats cancellation as terminal and skips them entirely. asOf is the
// tick's notion of "now"; adapters MAY ignore it (the rows are state-
// driven, not time-driven) but it is plumbed through so a future
// implementation can window on entered_state_at.
type CandidatesLister interface {
	ListCandidates(ctx context.Context, asOf time.Time, limit int) ([]Candidate, error)
}

// Saver persists a mutated DunningState. The actorID is recorded by the
// master_ops audit trigger. Implementations MUST upsert on
// subscription_id (UNIQUE in migration 0102).
type Saver interface {
	Save(ctx context.Context, d *dunning.DunningState, actorID uuid.UUID) error
}

// PolicyResolver maps a planID to the [dunning.Policy] in force for
// that plan. The default implementation returns [dunning.DefaultPolicy]
// for every plan; a future plan-level override (plans.dunning JSONB
// column) can ship without changing the worker.
type PolicyResolver func(planID uuid.UUID) dunning.Policy

// DefaultPolicyResolver returns [dunning.DefaultPolicy] for every plan.
// Wired as the fallback when no resolver is supplied in Config.
func DefaultPolicyResolver(uuid.UUID) dunning.Policy { return dunning.DefaultPolicy }
