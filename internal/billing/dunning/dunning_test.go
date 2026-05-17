package dunning_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing/dunning"
)

var (
	now       = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	dueDate   = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC) // 30+ days before now
	reasonOK  = "incident reparation per ticket SIN-99999"
	reasonBad = "too short"
)

func TestNewDunningState(t *testing.T) {
	tenant := uuid.New()
	sub := uuid.New()

	tests := []struct {
		name    string
		tenant  uuid.UUID
		sub     uuid.UUID
		wantErr error
	}{
		{name: "valid", tenant: tenant, sub: sub},
		{name: "zero tenant", tenant: uuid.Nil, sub: sub, wantErr: dunning.ErrZeroTenant},
		{name: "zero subscription", tenant: tenant, sub: uuid.Nil, wantErr: dunning.ErrZeroSubscription},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := dunning.NewDunningState(tc.tenant, tc.sub, now)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got err %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d.State() != dunning.StateCurrent {
				t.Errorf("new row state = %s, want current", d.State())
			}
			if d.TenantID() != tc.tenant {
				t.Errorf("tenant id mismatch")
			}
			if d.SubscriptionID() != tc.sub {
				t.Errorf("subscription id mismatch")
			}
			if !d.EnteredStateAt().Equal(now) {
				t.Errorf("entered_state_at = %s, want %s", d.EnteredStateAt(), now)
			}
			if d.LastInvoiceID() != uuid.Nil {
				t.Errorf("last_invoice_id should be nil on fresh row, got %s", d.LastInvoiceID())
			}
			if d.OverrideUntil() != nil {
				t.Errorf("override_until should be nil on fresh row")
			}
			if d.OverrideReason() != "" {
				t.Errorf("override_reason should be empty on fresh row")
			}
			if d.ID() == uuid.Nil {
				t.Errorf("id should be generated, got uuid.Nil")
			}
		})
	}
}

func TestHydrateDunningState(t *testing.T) {
	id := uuid.New()
	tenant := uuid.New()
	sub := uuid.New()
	invID := uuid.New()
	until := now.Add(7 * 24 * time.Hour)

	d := dunning.HydrateDunningState(
		id, tenant, sub,
		dunning.StateSuspendedOutbound,
		now,
		invID,
		&until,
		reasonOK,
	)

	if d.ID() != id {
		t.Errorf("ID mismatch")
	}
	if d.TenantID() != tenant {
		t.Errorf("TenantID mismatch")
	}
	if d.SubscriptionID() != sub {
		t.Errorf("SubscriptionID mismatch")
	}
	if d.State() != dunning.StateSuspendedOutbound {
		t.Errorf("state mismatch")
	}
	if d.LastInvoiceID() != invID {
		t.Errorf("LastInvoiceID mismatch")
	}
	if d.OverrideUntil() == nil || !d.OverrideUntil().Equal(until) {
		t.Errorf("OverrideUntil mismatch")
	}
	if d.OverrideReason() != reasonOK {
		t.Errorf("OverrideReason mismatch")
	}
}

func TestDunningState_HasActiveOverride(t *testing.T) {
	tenant := uuid.New()
	sub := uuid.New()
	d, err := dunning.NewDunningState(tenant, sub, now)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	if d.HasActiveOverride(now) {
		t.Errorf("fresh row should not have active override")
	}

	if err := d.ApplyOverride(now.Add(48*time.Hour), reasonOK, now); err != nil {
		t.Fatalf("ApplyOverride: %v", err)
	}
	if !d.HasActiveOverride(now) {
		t.Errorf("override should be active immediately after apply")
	}
	// After until has passed, it is no longer active.
	if d.HasActiveOverride(now.Add(72 * time.Hour)) {
		t.Errorf("override should be inactive after until")
	}
}

func TestDunningState_MarkPaid(t *testing.T) {
	t.Run("from warn returns to current and clears state", func(t *testing.T) {
		d := freshAt(t, dunning.StateWarn, uuid.New())
		later := now.Add(time.Hour)
		if err := d.MarkPaid(later); err != nil {
			t.Fatalf("MarkPaid: %v", err)
		}
		if d.State() != dunning.StateCurrent {
			t.Errorf("state = %s, want current", d.State())
		}
		if !d.EnteredStateAt().Equal(later) {
			t.Errorf("entered_state_at not updated")
		}
		if d.LastInvoiceID() != uuid.Nil {
			t.Errorf("last_invoice_id should clear, got %s", d.LastInvoiceID())
		}
	})

	t.Run("from suspended_full returns to current", func(t *testing.T) {
		d := freshAt(t, dunning.StateSuspendedFull, uuid.New())
		if err := d.MarkPaid(now); err != nil {
			t.Fatalf("MarkPaid: %v", err)
		}
		if d.State() != dunning.StateCurrent {
			t.Errorf("state = %s, want current", d.State())
		}
	})

	t.Run("from current is idempotent", func(t *testing.T) {
		d, _ := dunning.NewDunningState(uuid.New(), uuid.New(), now)
		later := now.Add(time.Hour)
		if err := d.MarkPaid(later); err != nil {
			t.Fatalf("MarkPaid: %v", err)
		}
		if d.State() != dunning.StateCurrent {
			t.Errorf("state = %s, want current", d.State())
		}
		if !d.EnteredStateAt().Equal(later) {
			t.Errorf("entered_state_at should update even when already current")
		}
	})

	t.Run("from cancelled is rejected", func(t *testing.T) {
		d := freshAt(t, dunning.StateCancelled, uuid.New())
		if err := d.MarkPaid(now); !errors.Is(err, dunning.ErrInvalidTransition) {
			t.Fatalf("got err %v, want ErrInvalidTransition", err)
		}
		if d.State() != dunning.StateCancelled {
			t.Errorf("state mutated despite rejection")
		}
	})

	t.Run("clears active override", func(t *testing.T) {
		d, _ := dunning.NewDunningState(uuid.New(), uuid.New(), now)
		if err := d.ApplyOverride(now.Add(48*time.Hour), reasonOK, now); err != nil {
			t.Fatalf("ApplyOverride: %v", err)
		}
		if err := d.MarkPaid(now); err != nil {
			t.Fatalf("MarkPaid: %v", err)
		}
		if d.OverrideUntil() != nil || d.OverrideReason() != "" {
			t.Errorf("override should clear, got until=%v reason=%q", d.OverrideUntil(), d.OverrideReason())
		}
	})
}

func TestDunningState_Escalate_Default(t *testing.T) {
	invID := uuid.New()
	policy := dunning.DefaultPolicy

	tests := []struct {
		name        string
		startState  dunning.State
		evalNow     time.Time
		wantMoved   bool
		wantState   dunning.State
		wantInvoice uuid.UUID // expected LastInvoiceID after call
	}{
		{
			name:        "current to warn at D+1",
			startState:  dunning.StateCurrent,
			evalNow:     dueDate.Add(24 * time.Hour),
			wantMoved:   true,
			wantState:   dunning.StateWarn,
			wantInvoice: invID,
		},
		{
			name:       "current stays current before D+1",
			startState: dunning.StateCurrent,
			evalNow:    dueDate.Add(23 * time.Hour),
			wantMoved:  false,
			wantState:  dunning.StateCurrent,
		},
		{
			name:        "current jumps to suspended_full at D+35 (skip levels)",
			startState:  dunning.StateCurrent,
			evalNow:     dueDate.Add(35 * 24 * time.Hour),
			wantMoved:   true,
			wantState:   dunning.StateSuspendedFull,
			wantInvoice: invID,
		},
		{
			name:        "current to cancelled at D+90",
			startState:  dunning.StateCurrent,
			evalNow:     dueDate.Add(90 * 24 * time.Hour),
			wantMoved:   true,
			wantState:   dunning.StateCancelled,
			wantInvoice: invID,
		},
		{
			name:        "warn to suspended_outbound at D+7",
			startState:  dunning.StateWarn,
			evalNow:     dueDate.Add(7 * 24 * time.Hour),
			wantMoved:   true,
			wantState:   dunning.StateSuspendedOutbound,
			wantInvoice: invID,
		},
		{
			name:       "warn stays warn at D+6",
			startState: dunning.StateWarn,
			evalNow:    dueDate.Add(6 * 24 * time.Hour),
			wantMoved:  false,
			wantState:  dunning.StateWarn,
		},
		{
			name:        "suspended_outbound to suspended_full at D+30",
			startState:  dunning.StateSuspendedOutbound,
			evalNow:     dueDate.Add(30 * 24 * time.Hour),
			wantMoved:   true,
			wantState:   dunning.StateSuspendedFull,
			wantInvoice: invID,
		},
		{
			name:        "suspended_full to cancelled at D+90",
			startState:  dunning.StateSuspendedFull,
			evalNow:     dueDate.Add(90 * 24 * time.Hour),
			wantMoved:   true,
			wantState:   dunning.StateCancelled,
			wantInvoice: invID,
		},
		{
			name:       "no downgrade — warn at D-1 stays warn",
			startState: dunning.StateWarn,
			evalNow:    dueDate.Add(-24 * time.Hour),
			wantMoved:  false,
			wantState:  dunning.StateWarn,
		},
		{
			name:       "cancelled is terminal",
			startState: dunning.StateCancelled,
			evalNow:    dueDate.Add(120 * 24 * time.Hour),
			wantMoved:  false,
			wantState:  dunning.StateCancelled,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := freshAt(t, tc.startState, uuid.Nil)
			moved, err := d.Escalate(tc.evalNow, policy, invID, dueDate, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if moved != tc.wantMoved {
				t.Errorf("moved = %v, want %v", moved, tc.wantMoved)
			}
			if d.State() != tc.wantState {
				t.Errorf("state = %s, want %s", d.State(), tc.wantState)
			}
			if d.LastInvoiceID() != tc.wantInvoice {
				t.Errorf("last_invoice_id = %s, want %s", d.LastInvoiceID(), tc.wantInvoice)
			}
			if tc.wantMoved && !d.EnteredStateAt().Equal(tc.evalNow) {
				t.Errorf("entered_state_at not updated on transition")
			}
		})
	}
}

func TestDunningState_Escalate_RejectsInvalidPolicy(t *testing.T) {
	d, _ := dunning.NewDunningState(uuid.New(), uuid.New(), now)
	bad := dunning.Policy{WarnDays: 0}
	moved, err := d.Escalate(now, bad, uuid.New(), dueDate, nil)
	if !errors.Is(err, dunning.ErrInvalidPolicy) {
		t.Fatalf("got err %v, want ErrInvalidPolicy", err)
	}
	if moved {
		t.Errorf("moved should be false on policy error")
	}
}

func TestDunningState_Escalate_RequiresInvoiceOnTransition(t *testing.T) {
	d, _ := dunning.NewDunningState(uuid.New(), uuid.New(), now)
	// D+1 would escalate; we pass uuid.Nil → must reject.
	moved, err := d.Escalate(
		dueDate.Add(24*time.Hour),
		dunning.DefaultPolicy,
		uuid.Nil,
		dueDate,
		nil,
	)
	if !errors.Is(err, dunning.ErrZeroInvoice) {
		t.Fatalf("got err %v, want ErrZeroInvoice", err)
	}
	if moved {
		t.Errorf("moved should be false on invoice-id rejection")
	}
	if d.State() != dunning.StateCurrent {
		t.Errorf("state mutated despite rejection")
	}
}

func TestDunningState_Escalate_RespectsActiveOverride(t *testing.T) {
	d, _ := dunning.NewDunningState(uuid.New(), uuid.New(), now)
	invID := uuid.New()
	// 40 days past due would normally land in suspended_full.
	evalNow := dueDate.Add(40 * 24 * time.Hour)
	override := &dunning.Override{
		Until:  evalNow.Add(48 * time.Hour),
		Reason: reasonOK,
	}
	moved, err := d.Escalate(evalNow, dunning.DefaultPolicy, invID, dueDate, override)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if moved {
		t.Errorf("moved = true, want false (override is active)")
	}
	if d.State() != dunning.StateCurrent {
		t.Errorf("state mutated despite active override")
	}
}

func TestDunningState_Escalate_ExpiredOverrideAllowsTransition(t *testing.T) {
	d, _ := dunning.NewDunningState(uuid.New(), uuid.New(), now)
	invID := uuid.New()
	evalNow := dueDate.Add(15 * 24 * time.Hour) // > 7 days → suspended_outbound
	override := &dunning.Override{
		Until:  evalNow.Add(-1 * time.Second), // expired
		Reason: reasonOK,
	}
	moved, err := d.Escalate(evalNow, dunning.DefaultPolicy, invID, dueDate, override)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !moved {
		t.Errorf("expected escalation despite expired override")
	}
	if d.State() != dunning.StateSuspendedOutbound {
		t.Errorf("state = %s, want suspended_outbound", d.State())
	}
}

func TestDunningState_ApplyOverride(t *testing.T) {
	until := now.Add(30 * 24 * time.Hour)

	tests := []struct {
		name       string
		startState dunning.State
		until      time.Time
		reason     string
		wantErr    error
		wantState  dunning.State
	}{
		{
			name:       "from current — no state change, override recorded",
			startState: dunning.StateCurrent,
			until:      until,
			reason:     reasonOK,
			wantState:  dunning.StateCurrent,
		},
		{
			name:       "from warn — resets to current",
			startState: dunning.StateWarn,
			until:      until,
			reason:     reasonOK,
			wantState:  dunning.StateCurrent,
		},
		{
			name:       "from suspended_outbound — resets to current",
			startState: dunning.StateSuspendedOutbound,
			until:      until,
			reason:     reasonOK,
			wantState:  dunning.StateCurrent,
		},
		{
			name:       "from suspended_full — resets to current",
			startState: dunning.StateSuspendedFull,
			until:      until,
			reason:     reasonOK,
			wantState:  dunning.StateCurrent,
		},
		{
			name:       "from cancelled rejected",
			startState: dunning.StateCancelled,
			until:      until,
			reason:     reasonOK,
			wantErr:    dunning.ErrInvalidTransition,
			wantState:  dunning.StateCancelled,
		},
		{
			name:       "reason too short",
			startState: dunning.StateWarn,
			until:      until,
			reason:     reasonBad,
			wantErr:    dunning.ErrOverrideReasonTooShort,
			wantState:  dunning.StateWarn,
		},
		{
			name:       "until equals now is rejected",
			startState: dunning.StateCurrent,
			until:      now,
			reason:     reasonOK,
			wantErr:    dunning.ErrOverrideUntilInPast,
			wantState:  dunning.StateCurrent,
		},
		{
			name:       "until in past is rejected",
			startState: dunning.StateCurrent,
			until:      now.Add(-1 * time.Second),
			reason:     reasonOK,
			wantErr:    dunning.ErrOverrideUntilInPast,
			wantState:  dunning.StateCurrent,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := freshAt(t, tc.startState, uuid.New())
			err := d.ApplyOverride(tc.until, tc.reason, now)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got err %v, want %v", err, tc.wantErr)
				}
				if d.State() != tc.wantState {
					t.Errorf("state mutated despite rejection: got %s, want %s", d.State(), tc.wantState)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d.State() != tc.wantState {
				t.Errorf("state = %s, want %s", d.State(), tc.wantState)
			}
			if d.OverrideUntil() == nil || !d.OverrideUntil().Equal(tc.until) {
				t.Errorf("override_until = %v, want %v", d.OverrideUntil(), tc.until)
			}
			if d.OverrideReason() != tc.reason {
				t.Errorf("override_reason = %q, want %q", d.OverrideReason(), tc.reason)
			}
			if tc.startState != dunning.StateCurrent {
				// reset path also clears last_invoice_id
				if d.LastInvoiceID() != uuid.Nil {
					t.Errorf("last_invoice_id not cleared on reset")
				}
			}
		})
	}
}

func TestDunningState_ClearOverride(t *testing.T) {
	d, _ := dunning.NewDunningState(uuid.New(), uuid.New(), now)
	if err := d.ApplyOverride(now.Add(48*time.Hour), reasonOK, now); err != nil {
		t.Fatalf("ApplyOverride: %v", err)
	}
	if d.OverrideUntil() == nil {
		t.Fatalf("setup: override should be set")
	}
	d.ClearOverride()
	if d.OverrideUntil() != nil || d.OverrideReason() != "" {
		t.Errorf("override should be clear, got until=%v reason=%q", d.OverrideUntil(), d.OverrideReason())
	}
	// Idempotent
	d.ClearOverride()
	if d.OverrideUntil() != nil {
		t.Errorf("ClearOverride should be idempotent")
	}
}

// freshAt builds a DunningState already in startState. The state
// machine has no public way to land in a non-current state without
// driving escalation, so the helper uses HydrateDunningState.
func freshAt(t *testing.T, startState dunning.State, lastInvoiceID uuid.UUID) *dunning.DunningState {
	t.Helper()
	return dunning.HydrateDunningState(
		uuid.New(),
		uuid.New(),
		uuid.New(),
		startState,
		now.Add(-time.Hour),
		lastInvoiceID,
		nil,
		"",
	)
}
