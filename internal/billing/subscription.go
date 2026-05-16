package billing

import (
	"time"

	"github.com/google/uuid"
)

// SubscriptionStatus is the lifecycle state of a Subscription.
type SubscriptionStatus string

const (
	// SubscriptionStatusActive is the only status under which a tenant
	// receives service. The database enforces at most one active subscription
	// per tenant via the partial unique index subscription_one_active_per_tenant_idx.
	SubscriptionStatusActive SubscriptionStatus = "active"

	// SubscriptionStatusCancelled means the subscription has been terminated.
	// Cancelled rows remain in place for audit; a new active subscription may
	// coexist alongside them.
	SubscriptionStatusCancelled SubscriptionStatus = "cancelled"
)

// Subscription tracks a tenant's current plan assignment. One active
// subscription per tenant is enforced at the database level (partial UNIQUE
// on tenant_id WHERE status='active'). Writes are master-only; tenants can
// read their own subscription via the runtime role.
type Subscription struct {
	id                 uuid.UUID
	tenantID           uuid.UUID
	planID             uuid.UUID
	status             SubscriptionStatus
	currentPeriodStart time.Time
	currentPeriodEnd   time.Time
	createdAt          time.Time
	updatedAt          time.Time
}

// NewSubscription constructs an active Subscription for tenantID on planID.
// periodEnd must be strictly after periodStart; both planID and tenantID must
// be non-nil. The subscription starts in the Active state.
func NewSubscription(tenantID, planID uuid.UUID, periodStart, periodEnd, now time.Time) (*Subscription, error) {
	if tenantID == uuid.Nil {
		return nil, ErrZeroTenant
	}
	if planID == uuid.Nil {
		return nil, ErrInvalidTransition
	}
	if !periodEnd.After(periodStart) {
		return nil, ErrInvalidTransition
	}
	return &Subscription{
		id:                 uuid.New(),
		tenantID:           tenantID,
		planID:             planID,
		status:             SubscriptionStatusActive,
		currentPeriodStart: periodStart,
		currentPeriodEnd:   periodEnd,
		createdAt:          now,
		updatedAt:          now,
	}, nil
}

// HydrateSubscription reconstructs a Subscription from durable state.
// Only adapters should call this; it bypasses the invariants enforced by
// NewSubscription because the database already vetted them.
func HydrateSubscription(
	id, tenantID, planID uuid.UUID,
	status SubscriptionStatus,
	periodStart, periodEnd,
	createdAt, updatedAt time.Time,
) *Subscription {
	return &Subscription{
		id:                 id,
		tenantID:           tenantID,
		planID:             planID,
		status:             status,
		currentPeriodStart: periodStart,
		currentPeriodEnd:   periodEnd,
		createdAt:          createdAt,
		updatedAt:          updatedAt,
	}
}

func (s *Subscription) ID() uuid.UUID                 { return s.id }
func (s *Subscription) TenantID() uuid.UUID           { return s.tenantID }
func (s *Subscription) PlanID() uuid.UUID             { return s.planID }
func (s *Subscription) Status() SubscriptionStatus    { return s.status }
func (s *Subscription) CurrentPeriodStart() time.Time { return s.currentPeriodStart }
func (s *Subscription) CurrentPeriodEnd() time.Time   { return s.currentPeriodEnd }
func (s *Subscription) CreatedAt() time.Time          { return s.createdAt }
func (s *Subscription) UpdatedAt() time.Time          { return s.updatedAt }

// Cancel transitions the subscription from Active to Cancelled. Returns
// ErrInvalidTransition if the subscription is already cancelled.
func (s *Subscription) Cancel(now time.Time) error {
	if s.status != SubscriptionStatusActive {
		return ErrInvalidTransition
	}
	s.status = SubscriptionStatusCancelled
	s.updatedAt = now
	return nil
}
