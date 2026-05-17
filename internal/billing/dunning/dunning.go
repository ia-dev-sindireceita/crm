// Package dunning is the subscription-level inadimplência (past-due)
// state machine for SIN-62957 / Fase 4.
//
// The domain owns four concepts:
//
//   - State          — the enum {current, warn, suspended_outbound,
//     suspended_full, cancelled}.
//   - Policy         — the per-plan thresholds in days
//     (warn/outbound/readonly/cancel).
//   - DunningState   — the aggregate root: one row per subscription.
//   - Override       — a value type capturing an active courtesy-grant
//     reprieve (free_subscription_period).
//
// Transitions are driven by three external signals:
//
//   - Time: the cron worker (delivered in SIN-62965 / C14) calls
//     Escalate with the current invoice due date; the entity returns
//     whether it advanced.
//   - Payment confirmation: MarkPaid drops back to State current.
//   - Administrative override: ApplyOverride records the
//     CourtesyGrant.kind=free_subscription_period reprieve.
//
// The package follows the "Domain pure" rule (issue AC #3): no
// database/sql, pgx, net/http or PSP imports. Persistence lives behind
// the DunningRepository port; CourtesyOverride is the read-only port
// adapters fulfil from the wallet/master_grant store.
//
// Reference decision: board ratification D1 in
// [SIN-62204](/SIN/issues/SIN-62204) — "Bloqueio escalonado" with the
// {1, 7, 30, 90} default policy and master override via
// CourtesyGrant.kind=free_subscription_period.
package dunning

import (
	"time"

	"github.com/google/uuid"
)

// DunningState is the dunning-state aggregate root. One row per
// subscription, identified by SubscriptionID (UNIQUE in the database;
// see migration 0102 subscription_dunning_states).
//
// Invariants enforced by the constructors and transitions:
//
//   - tenantID and subscriptionID are non-nil uuids.
//   - state is one of the five canonical values (see State).
//   - override_until and override_reason are both set or both clear;
//     when set, override_reason has length ≥ 10 (mirrors the DB CHECK
//     constraint subscription_dunning_states_override_consistency).
//   - state=cancelled is terminal — no further escalation, no override
//     can be applied; MarkPaid is the only legal mutation that may
//     succeed (and only because we want to keep the row's audit chain
//     coherent if a payment somehow lands on a cancelled subscription;
//     the receipt itself stays unchanged in the invoice domain).
//
// All accessors return value types; the entity owns mutation.
type DunningState struct {
	id             uuid.UUID
	tenantID       uuid.UUID
	subscriptionID uuid.UUID
	state          State
	enteredStateAt time.Time
	lastInvoiceID  uuid.UUID
	overrideUntil  *time.Time
	overrideReason string
}

// NewDunningState constructs a fresh dunning row for subscriptionID in
// State current. Callers create it at subscription provisioning time
// so the state machine always has a row to read.
//
// Returns ErrZeroTenant for uuid.Nil tenantID and ErrZeroSubscription
// for uuid.Nil subscriptionID.
func NewDunningState(tenantID, subscriptionID uuid.UUID, now time.Time) (*DunningState, error) {
	if tenantID == uuid.Nil {
		return nil, ErrZeroTenant
	}
	if subscriptionID == uuid.Nil {
		return nil, ErrZeroSubscription
	}
	return &DunningState{
		id:             uuid.New(),
		tenantID:       tenantID,
		subscriptionID: subscriptionID,
		state:          StateCurrent,
		enteredStateAt: now,
	}, nil
}

// HydrateDunningState rebuilds a DunningState from durable storage.
// Only adapters should call this; it bypasses the invariants enforced
// by NewDunningState because the database has already vetted them.
//
// lastInvoiceID is uuid.Nil for rows that never escalated. overrideUntil
// is nil for rows without an active grant; overrideReason is "" in the
// same case.
func HydrateDunningState(
	id, tenantID, subscriptionID uuid.UUID,
	state State,
	enteredStateAt time.Time,
	lastInvoiceID uuid.UUID,
	overrideUntil *time.Time,
	overrideReason string,
) *DunningState {
	return &DunningState{
		id:             id,
		tenantID:       tenantID,
		subscriptionID: subscriptionID,
		state:          state,
		enteredStateAt: enteredStateAt,
		lastInvoiceID:  lastInvoiceID,
		overrideUntil:  overrideUntil,
		overrideReason: overrideReason,
	}
}

// ID returns the dunning row's primary key.
func (d *DunningState) ID() uuid.UUID { return d.id }

// TenantID returns the owning tenant. Denormalised onto the row so RLS
// can enforce tenant isolation without joining through subscription.
func (d *DunningState) TenantID() uuid.UUID { return d.tenantID }

// SubscriptionID returns the subscription this row tracks.
func (d *DunningState) SubscriptionID() uuid.UUID { return d.subscriptionID }

// State returns the current state.
func (d *DunningState) State() State { return d.state }

// EnteredStateAt returns the timestamp the row entered its current
// state. Used by the cron to compute time-in-state for observability.
func (d *DunningState) EnteredStateAt() time.Time { return d.enteredStateAt }

// LastInvoiceID returns the invoice whose past-due window drove the
// most recent escalation, or uuid.Nil if the row has never escalated.
func (d *DunningState) LastInvoiceID() uuid.UUID { return d.lastInvoiceID }

// OverrideUntil returns the timestamp at which an active courtesy
// override expires, or nil if no override is active.
func (d *DunningState) OverrideUntil() *time.Time { return d.overrideUntil }

// OverrideReason returns the human-readable reason recorded with the
// active override, or "" if no override is active.
func (d *DunningState) OverrideReason() string { return d.overrideReason }

// HasActiveOverride reports whether an override is currently in effect
// (override_until > now).
func (d *DunningState) HasActiveOverride(now time.Time) bool {
	return d.overrideUntil != nil && d.overrideUntil.After(now)
}

// MarkPaid transitions to State current from any non-cancelled state
// and clears the override and last-invoice slots. Returns
// ErrInvalidTransition if the row is already in State cancelled —
// dunning treats cancellation as terminal so a stray payment after
// cancellation must be handled in the invoice domain, not here.
//
// Idempotent at State current: calling MarkPaid on an already-current
// row updates entered_state_at to now (so callers can use this as the
// "ack" of payment) but never errors.
func (d *DunningState) MarkPaid(now time.Time) error {
	if d.state == StateCancelled {
		return ErrInvalidTransition
	}
	d.state = StateCurrent
	d.enteredStateAt = now
	d.lastInvoiceID = uuid.Nil
	d.overrideUntil = nil
	d.overrideReason = ""
	return nil
}

// Escalate evaluates the dunning state at now given the policy, the
// last invoice due date, and any active override. If a transition
// applies, it mutates the entity and returns true; otherwise it
// returns false. Returns an error only on invalid inputs.
//
// Rules (matching D1 / SIN-62204):
//
//   - If the row is in StateCancelled, no-op (terminal). Returns
//     (false, nil).
//   - If override is non-nil and override.Until > now, no-op. The
//     override pauses escalation; cron resumes on its next tick after
//     the override expires.
//   - Compute target = policy.StateForDaysPastDue(daysSince).
//   - If target.Severity() > current.Severity(), transition and set
//     LastInvoiceID + EnteredStateAt. We do NOT downgrade in this
//     method; downgrade is the job of MarkPaid.
//   - daysSince is computed as floor((now - dueDate) / 24h), clamped
//     to 0 when now is before dueDate.
//
// invoiceID is the invoice that drove the past-due window; uuid.Nil is
// rejected with ErrZeroInvoice when target > current.
func (d *DunningState) Escalate(
	now time.Time,
	policy Policy,
	invoiceID uuid.UUID,
	dueDate time.Time,
	override *Override,
) (bool, error) {
	if err := policy.Validate(); err != nil {
		return false, err
	}
	if d.state == StateCancelled {
		return false, nil
	}
	if override != nil && override.Until.After(now) {
		return false, nil
	}

	days := daysSince(dueDate, now)
	target := policy.StateForDaysPastDue(days)
	if target.Severity() <= d.state.Severity() {
		return false, nil
	}
	if invoiceID == uuid.Nil {
		return false, ErrZeroInvoice
	}
	d.state = target
	d.enteredStateAt = now
	d.lastInvoiceID = invoiceID
	return true, nil
}

// ApplyOverride records an administrative reprieve (master granted a
// free_subscription_period via the wallet domain). Semantics:
//
//   - reason must be ≥ 10 characters (mirrors the DB CHECK).
//   - until must be strictly after now (no retroactive overrides).
//   - The row may NOT be in StateCancelled — cancellation is terminal
//     in this domain.
//   - If the row was above StateCurrent (warn / suspended_*), it is
//     reset to StateCurrent and EnteredStateAt is updated. This
//     matches AC #2: "reset/estende o estado conforme ADR-0086". The
//     grant pays for the current period, so the tenant returns to a
//     clean slate; if the override expires unpaid, the cron resumes
//     escalation based on the next invoice's due date.
//   - LastInvoiceID is cleared because the past-due invoice is
//     considered "forgiven" for the override window.
func (d *DunningState) ApplyOverride(until time.Time, reason string, now time.Time) error {
	if d.state == StateCancelled {
		return ErrInvalidTransition
	}
	if len(reason) < 10 {
		return ErrOverrideReasonTooShort
	}
	if !until.After(now) {
		return ErrOverrideUntilInPast
	}
	d.overrideUntil = &until
	d.overrideReason = reason
	if d.state != StateCurrent {
		d.state = StateCurrent
		d.enteredStateAt = now
		d.lastInvoiceID = uuid.Nil
	}
	return nil
}

// ClearOverride removes an active override (e.g. revocation of the
// CourtesyGrant). It does NOT change state — escalation resumes on the
// next cron tick based on the actual past-due window.
//
// Idempotent: clearing an already-clear override is a no-op.
func (d *DunningState) ClearOverride() {
	d.overrideUntil = nil
	d.overrideReason = ""
}

// daysSince returns floor((later - earlier) / 24h), clamped to 0 when
// later is before earlier. Pulled out as a free function so the
// rounding rule has a single home and is testable in isolation.
func daysSince(earlier, later time.Time) int {
	if !later.After(earlier) {
		return 0
	}
	const day = 24 * time.Hour
	return int(later.Sub(earlier) / day)
}
