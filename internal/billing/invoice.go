package billing

import (
	"time"

	"github.com/google/uuid"
)

// InvoiceState is the lifecycle state of an Invoice.
type InvoiceState string

const (
	// InvoiceStatePending is the initial state; the invoice has been issued
	// but not yet settled.
	InvoiceStatePending InvoiceState = "pending"

	// InvoiceStatePaid means the invoice has been settled.
	InvoiceStatePaid InvoiceState = "paid"

	// InvoiceStateCancelledByMaster means a master operator explicitly voided
	// the invoice, recording a human-readable reason (≥10 chars). The
	// partial unique index on (tenant_id, period_start) excludes cancelled
	// rows, so a fresh invoice may be issued for the same period afterwards.
	InvoiceStateCancelledByMaster InvoiceState = "cancelled_by_master"
)

// Invoice represents a monthly billing row for a tenant's subscription.
// Writes are master-only; tenants can read their own invoices via the
// runtime role.
type Invoice struct {
	id              uuid.UUID
	tenantID        uuid.UUID
	subscriptionID  uuid.UUID
	periodStart     time.Time
	periodEnd       time.Time
	amountCentsBRL  int
	state           InvoiceState
	cancelledReason string
	createdAt       time.Time
	updatedAt       time.Time
}

// NewInvoice constructs a pending invoice for tenantID on subscriptionID.
// periodEnd must be strictly after periodStart; amountCentsBRL must be ≥ 0.
func NewInvoice(
	tenantID, subscriptionID uuid.UUID,
	periodStart, periodEnd time.Time,
	amountCentsBRL int,
	now time.Time,
) (*Invoice, error) {
	if tenantID == uuid.Nil {
		return nil, ErrZeroTenant
	}
	if subscriptionID == uuid.Nil {
		return nil, ErrInvalidTransition
	}
	if !periodEnd.After(periodStart) {
		return nil, ErrInvalidTransition
	}
	if amountCentsBRL < 0 {
		return nil, ErrInvalidTransition
	}
	return &Invoice{
		id:             uuid.New(),
		tenantID:       tenantID,
		subscriptionID: subscriptionID,
		periodStart:    periodStart,
		periodEnd:      periodEnd,
		amountCentsBRL: amountCentsBRL,
		state:          InvoiceStatePending,
		createdAt:      now,
		updatedAt:      now,
	}, nil
}

// HydrateInvoice reconstructs an Invoice from durable state.
// Only adapters should call this.
func HydrateInvoice(
	id, tenantID, subscriptionID uuid.UUID,
	periodStart, periodEnd time.Time,
	amountCentsBRL int,
	state InvoiceState,
	cancelledReason string,
	createdAt, updatedAt time.Time,
) *Invoice {
	return &Invoice{
		id:              id,
		tenantID:        tenantID,
		subscriptionID:  subscriptionID,
		periodStart:     periodStart,
		periodEnd:       periodEnd,
		amountCentsBRL:  amountCentsBRL,
		state:           state,
		cancelledReason: cancelledReason,
		createdAt:       createdAt,
		updatedAt:       updatedAt,
	}
}

func (inv *Invoice) ID() uuid.UUID             { return inv.id }
func (inv *Invoice) TenantID() uuid.UUID       { return inv.tenantID }
func (inv *Invoice) SubscriptionID() uuid.UUID { return inv.subscriptionID }
func (inv *Invoice) PeriodStart() time.Time    { return inv.periodStart }
func (inv *Invoice) PeriodEnd() time.Time      { return inv.periodEnd }
func (inv *Invoice) AmountCentsBRL() int       { return inv.amountCentsBRL }
func (inv *Invoice) State() InvoiceState       { return inv.state }
func (inv *Invoice) CancelledReason() string   { return inv.cancelledReason }
func (inv *Invoice) CreatedAt() time.Time      { return inv.createdAt }
func (inv *Invoice) UpdatedAt() time.Time      { return inv.updatedAt }

// MarkPaid transitions the invoice from Pending to Paid. Returns
// ErrInvalidTransition if the invoice is not pending.
func (inv *Invoice) MarkPaid(now time.Time) error {
	if inv.state != InvoiceStatePending {
		return ErrInvalidTransition
	}
	inv.state = InvoiceStatePaid
	inv.updatedAt = now
	return nil
}

// CancelByMaster transitions the invoice to CancelledByMaster from any
// non-cancelled state. reason must be at least 10 characters (mirrors the
// DB CHECK constraint in migration 0097). A paid invoice can be retroactively
// cancelled by a master operator with a documented reason.
func (inv *Invoice) CancelByMaster(reason string, now time.Time) error {
	if inv.state == InvoiceStateCancelledByMaster {
		return ErrInvalidTransition
	}
	if len(reason) < 10 {
		return ErrCancelReasonTooShort
	}
	inv.state = InvoiceStateCancelledByMaster
	inv.cancelledReason = reason
	inv.updatedAt = now
	return nil
}
