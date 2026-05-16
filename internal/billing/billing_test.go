package billing_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing"
)

// TestErrors verifies that all domain sentinel errors are non-nil and
// distinct, so callers can match them with errors.Is.
func TestErrors(t *testing.T) {
	errs := []error{
		billing.ErrInvalidTransition,
		billing.ErrInvoiceAlreadyExists,
		billing.ErrNotFound,
		billing.ErrZeroTenant,
		billing.ErrCancelReasonTooShort,
	}
	for i, a := range errs {
		if a == nil {
			t.Errorf("error[%d] is nil", i)
		}
		for j, b := range errs {
			if i != j && errors.Is(a, b) {
				t.Errorf("error[%d] and error[%d] are indistinguishable", i, j)
			}
		}
	}
}

// TestHydrateInvoice_Accessors verifies all getter methods return the
// values provided to HydrateInvoice. This is the main path adapters use
// to rebuild Invoice from durable state.
func TestHydrateInvoice_Accessors(t *testing.T) {
	id := uuid.New()
	tenantID := uuid.New()
	subscriptionID := uuid.New()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	created := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	const reason = "cancelled by operator"

	inv := billing.HydrateInvoice(
		id, tenantID, subscriptionID,
		start, end,
		9999,
		billing.InvoiceStateCancelledByMaster,
		reason,
		created, updated,
	)

	if inv.ID() != id {
		t.Error("ID mismatch")
	}
	if inv.TenantID() != tenantID {
		t.Error("TenantID mismatch")
	}
	if inv.SubscriptionID() != subscriptionID {
		t.Error("SubscriptionID mismatch")
	}
	if !inv.PeriodStart().Equal(start) {
		t.Error("PeriodStart mismatch")
	}
	if !inv.PeriodEnd().Equal(end) {
		t.Error("PeriodEnd mismatch")
	}
	if inv.AmountCentsBRL() != 9999 {
		t.Errorf("AmountCentsBRL mismatch: %d", inv.AmountCentsBRL())
	}
	if inv.State() != billing.InvoiceStateCancelledByMaster {
		t.Errorf("State mismatch: %s", inv.State())
	}
	if inv.CancelledReason() != reason {
		t.Errorf("CancelledReason mismatch: %q", inv.CancelledReason())
	}
	if !inv.CreatedAt().Equal(created) {
		t.Error("CreatedAt mismatch")
	}
	if !inv.UpdatedAt().Equal(updated) {
		t.Error("UpdatedAt mismatch")
	}
}

// TestPlan_Fields verifies Plan is a usable value type.
func TestPlan_Fields(t *testing.T) {
	p := billing.Plan{
		ID:                uuid.New(),
		Slug:              "pro",
		Name:              "Pro Plan",
		PriceCentsBRL:     4990,
		MonthlyTokenQuota: 1_000_000,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	if p.Slug != "pro" {
		t.Errorf("slug mismatch: %s", p.Slug)
	}
	if p.PriceCentsBRL != 4990 {
		t.Errorf("price mismatch: %d", p.PriceCentsBRL)
	}
}

// TestSubscription_Accessors exercises all getter methods on a hydrated
// Subscription to ensure they return the supplied values.
func TestSubscription_Accessors(t *testing.T) {
	id := uuid.New()
	tenantID := uuid.New()
	planID := uuid.New()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	created := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)

	sub := billing.HydrateSubscription(
		id, tenantID, planID,
		billing.SubscriptionStatusActive,
		start, end, created, updated,
	)

	if sub.ID() != id {
		t.Error("ID mismatch")
	}
	if sub.TenantID() != tenantID {
		t.Error("TenantID mismatch")
	}
	if sub.PlanID() != planID {
		t.Error("PlanID mismatch")
	}
	if sub.Status() != billing.SubscriptionStatusActive {
		t.Errorf("Status mismatch: %s", sub.Status())
	}
	if !sub.CurrentPeriodStart().Equal(start) {
		t.Error("CurrentPeriodStart mismatch")
	}
	if !sub.CurrentPeriodEnd().Equal(end) {
		t.Error("CurrentPeriodEnd mismatch")
	}
	if !sub.CreatedAt().Equal(created) {
		t.Error("CreatedAt mismatch")
	}
	if !sub.UpdatedAt().Equal(updated) {
		t.Error("UpdatedAt mismatch")
	}
}

// TestNewInvoice_Accessors exercises all getter methods on a freshly
// constructed Invoice.
func TestNewInvoice_Accessors(t *testing.T) {
	tenantID := uuid.New()
	subscriptionID := uuid.New()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	n := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	inv, err := billing.NewInvoice(tenantID, subscriptionID, start, end, 4990, n)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if inv.TenantID() != tenantID {
		t.Error("TenantID mismatch")
	}
	if inv.SubscriptionID() != subscriptionID {
		t.Error("SubscriptionID mismatch")
	}
	if !inv.PeriodStart().Equal(start) {
		t.Error("PeriodStart mismatch")
	}
	if !inv.PeriodEnd().Equal(end) {
		t.Error("PeriodEnd mismatch")
	}
	if inv.CancelledReason() != "" {
		t.Errorf("expected empty reason, got %q", inv.CancelledReason())
	}
	if !inv.CreatedAt().Equal(n) {
		t.Error("CreatedAt mismatch")
	}
}
