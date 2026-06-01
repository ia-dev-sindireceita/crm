package billing_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing"
)

var (
	now         = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	periodStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	periodEnd   = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
)

func TestNewSubscription(t *testing.T) {
	tenantID := uuid.New()
	planID := uuid.New()

	tests := []struct {
		name     string
		tenantID uuid.UUID
		planID   uuid.UUID
		start    time.Time
		end      time.Time
		wantErr  error
	}{
		{
			name:     "valid",
			tenantID: tenantID,
			planID:   planID,
			start:    periodStart,
			end:      periodEnd,
		},
		{
			name:     "zero tenant",
			tenantID: uuid.Nil,
			planID:   planID,
			start:    periodStart,
			end:      periodEnd,
			wantErr:  billing.ErrZeroTenant,
		},
		{
			name:     "nil plan",
			tenantID: tenantID,
			planID:   uuid.Nil,
			start:    periodStart,
			end:      periodEnd,
			wantErr:  billing.ErrInvalidTransition,
		},
		{
			name:     "end before start",
			tenantID: tenantID,
			planID:   planID,
			start:    periodEnd,
			end:      periodStart,
			wantErr:  billing.ErrInvalidTransition,
		},
		{
			name:     "end equals start",
			tenantID: tenantID,
			planID:   planID,
			start:    periodStart,
			end:      periodStart,
			wantErr:  billing.ErrInvalidTransition,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sub, err := billing.NewSubscription(tc.tenantID, tc.planID, tc.start, tc.end, now)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got err %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if sub.Status() != billing.SubscriptionStatusActive {
				t.Errorf("expected active, got %s", sub.Status())
			}
			if sub.TenantID() != tc.tenantID {
				t.Errorf("tenant id mismatch")
			}
			if sub.PlanID() != tc.planID {
				t.Errorf("plan id mismatch")
			}
		})
	}
}

func TestSubscription_Cancel(t *testing.T) {
	t.Run("active to cancelled", func(t *testing.T) {
		sub, _ := billing.NewSubscription(uuid.New(), uuid.New(), periodStart, periodEnd, now)
		if err := sub.Cancel(now); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sub.Status() != billing.SubscriptionStatusCancelled {
			t.Errorf("expected cancelled, got %s", sub.Status())
		}
	})

	t.Run("cancelled to cancelled is invalid", func(t *testing.T) {
		sub, _ := billing.NewSubscription(uuid.New(), uuid.New(), periodStart, periodEnd, now)
		_ = sub.Cancel(now)
		if err := sub.Cancel(now); !errors.Is(err, billing.ErrInvalidTransition) {
			t.Errorf("expected ErrInvalidTransition, got %v", err)
		}
	})
}

func TestSubscription_ExtendPeriod(t *testing.T) {
	t.Run("active subscription advances current_period_end", func(t *testing.T) {
		sub, _ := billing.NewSubscription(uuid.New(), uuid.New(), periodStart, periodEnd, now)
		originalEnd := sub.CurrentPeriodEnd()
		later := now.Add(time.Hour)
		if err := sub.ExtendPeriod(30*24*time.Hour, later); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sub.Status() != billing.SubscriptionStatusActive {
			t.Errorf("status changed: got %s, want active", sub.Status())
		}
		want := originalEnd.Add(30 * 24 * time.Hour)
		if !sub.CurrentPeriodEnd().Equal(want) {
			t.Errorf("current_period_end = %s, want %s", sub.CurrentPeriodEnd(), want)
		}
		if !sub.UpdatedAt().Equal(later) {
			t.Errorf("updated_at = %s, want %s", sub.UpdatedAt(), later)
		}
	})

	t.Run("zero duration is invalid", func(t *testing.T) {
		sub, _ := billing.NewSubscription(uuid.New(), uuid.New(), periodStart, periodEnd, now)
		if err := sub.ExtendPeriod(0, now); !errors.Is(err, billing.ErrInvalidTransition) {
			t.Errorf("ExtendPeriod(0): got %v, want ErrInvalidTransition", err)
		}
	})

	t.Run("negative duration is invalid", func(t *testing.T) {
		sub, _ := billing.NewSubscription(uuid.New(), uuid.New(), periodStart, periodEnd, now)
		if err := sub.ExtendPeriod(-time.Hour, now); !errors.Is(err, billing.ErrInvalidTransition) {
			t.Errorf("ExtendPeriod(-1h): got %v, want ErrInvalidTransition", err)
		}
	})

	t.Run("cancelled subscription cannot be extended", func(t *testing.T) {
		sub, _ := billing.NewSubscription(uuid.New(), uuid.New(), periodStart, periodEnd, now)
		_ = sub.Cancel(now)
		if err := sub.ExtendPeriod(24*time.Hour, now); !errors.Is(err, billing.ErrInvalidTransition) {
			t.Errorf("ExtendPeriod on cancelled: got %v, want ErrInvalidTransition", err)
		}
	})
}

func TestHydrateSubscription(t *testing.T) {
	id := uuid.New()
	tenantID := uuid.New()
	planID := uuid.New()

	sub := billing.HydrateSubscription(id, tenantID, planID,
		billing.SubscriptionStatusCancelled,
		periodStart, periodEnd, now, now)

	if sub.ID() != id {
		t.Error("id mismatch")
	}
	if sub.Status() != billing.SubscriptionStatusCancelled {
		t.Errorf("expected cancelled, got %s", sub.Status())
	}
}
