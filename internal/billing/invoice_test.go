package billing_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing"
)

func TestNewInvoice(t *testing.T) {
	tenantID := uuid.New()
	subID := uuid.New()

	tests := []struct {
		name           string
		tenantID       uuid.UUID
		subscriptionID uuid.UUID
		start          interface{} // use periodStart/periodEnd from subscription_test.go
		end            interface{}
		amount         int
		wantErr        error
	}{
		{name: "valid", tenantID: tenantID, subscriptionID: subID, amount: 4990},
		{name: "zero tenant", tenantID: uuid.Nil, subscriptionID: subID, amount: 4990, wantErr: billing.ErrZeroTenant},
		{name: "nil subscription", tenantID: tenantID, subscriptionID: uuid.Nil, amount: 4990, wantErr: billing.ErrInvalidTransition},
		{name: "negative amount", tenantID: tenantID, subscriptionID: subID, amount: -1, wantErr: billing.ErrInvalidTransition},
		{name: "zero amount ok", tenantID: tenantID, subscriptionID: subID, amount: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv, err := billing.NewInvoice(tc.tenantID, tc.subscriptionID, periodStart, periodEnd, tc.amount, now)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got err %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if inv.State() != billing.InvoiceStatePending {
				t.Errorf("expected pending, got %s", inv.State())
			}
			if inv.AmountCentsBRL() != tc.amount {
				t.Errorf("amount mismatch: got %d, want %d", inv.AmountCentsBRL(), tc.amount)
			}
		})
	}
}

func TestInvoice_MarkPaid(t *testing.T) {
	t.Run("pending to paid", func(t *testing.T) {
		inv, _ := billing.NewInvoice(uuid.New(), uuid.New(), periodStart, periodEnd, 4990, now)
		if err := inv.MarkPaid(now); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if inv.State() != billing.InvoiceStatePaid {
			t.Errorf("expected paid, got %s", inv.State())
		}
	})

	t.Run("paid to paid is invalid", func(t *testing.T) {
		inv, _ := billing.NewInvoice(uuid.New(), uuid.New(), periodStart, periodEnd, 4990, now)
		_ = inv.MarkPaid(now)
		if err := inv.MarkPaid(now); !errors.Is(err, billing.ErrInvalidTransition) {
			t.Errorf("expected ErrInvalidTransition, got %v", err)
		}
	})

	t.Run("cancelled to paid is invalid", func(t *testing.T) {
		inv, _ := billing.NewInvoice(uuid.New(), uuid.New(), periodStart, periodEnd, 4990, now)
		_ = inv.CancelByMaster("cancelled for testing purposes here", now)
		if err := inv.MarkPaid(now); !errors.Is(err, billing.ErrInvalidTransition) {
			t.Errorf("expected ErrInvalidTransition, got %v", err)
		}
	})
}

func TestInvoice_CancelByMaster(t *testing.T) {
	const longReason = "cancelled for testing purposes"

	t.Run("pending to cancelled", func(t *testing.T) {
		inv, _ := billing.NewInvoice(uuid.New(), uuid.New(), periodStart, periodEnd, 4990, now)
		if err := inv.CancelByMaster(longReason, now); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if inv.State() != billing.InvoiceStateCancelledByMaster {
			t.Errorf("expected cancelled_by_master, got %s", inv.State())
		}
		if inv.CancelledReason() != longReason {
			t.Errorf("reason mismatch")
		}
	})

	t.Run("paid to cancelled allowed", func(t *testing.T) {
		inv, _ := billing.NewInvoice(uuid.New(), uuid.New(), periodStart, periodEnd, 4990, now)
		_ = inv.MarkPaid(now)
		if err := inv.CancelByMaster(longReason, now); err != nil {
			t.Fatalf("paid invoice should be cancellable: %v", err)
		}
	})

	t.Run("already cancelled is invalid", func(t *testing.T) {
		inv, _ := billing.NewInvoice(uuid.New(), uuid.New(), periodStart, periodEnd, 4990, now)
		_ = inv.CancelByMaster(longReason, now)
		if err := inv.CancelByMaster(longReason, now); !errors.Is(err, billing.ErrInvalidTransition) {
			t.Errorf("expected ErrInvalidTransition, got %v", err)
		}
	})

	t.Run("reason too short", func(t *testing.T) {
		inv, _ := billing.NewInvoice(uuid.New(), uuid.New(), periodStart, periodEnd, 4990, now)
		if err := inv.CancelByMaster("short", now); !errors.Is(err, billing.ErrCancelReasonTooShort) {
			t.Errorf("expected ErrCancelReasonTooShort, got %v", err)
		}
	})

	t.Run("exactly 10 chars ok", func(t *testing.T) {
		inv, _ := billing.NewInvoice(uuid.New(), uuid.New(), periodStart, periodEnd, 4990, now)
		if err := inv.CancelByMaster("1234567890", now); err != nil {
			t.Fatalf("10-char reason should be accepted: %v", err)
		}
	})
}

func TestNewInvoice_EndBeforeStart(t *testing.T) {
	_, err := billing.NewInvoice(uuid.New(), uuid.New(), periodEnd, periodStart, 100, now)
	if !errors.Is(err, billing.ErrInvalidTransition) {
		t.Errorf("expected ErrInvalidTransition, got %v", err)
	}
}
