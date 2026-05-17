package dunning_test

import (
	"errors"
	"testing"

	"github.com/pericles-luz/crm/internal/billing/dunning"
)

func TestDefaultPolicy_MatchesD1(t *testing.T) {
	// AC #1: default {1, 7, 30, 90} exactly as ratified in D1
	// (SIN-62204) and reflected in the migration check constraints.
	want := dunning.Policy{
		WarnDays:          1,
		OutboundBlockDays: 7,
		ReadonlyDays:      30,
		CancelDays:        90,
	}
	if dunning.DefaultPolicy != want {
		t.Errorf("DefaultPolicy = %+v, want %+v", dunning.DefaultPolicy, want)
	}
}

func TestPolicy_Validate(t *testing.T) {
	tests := []struct {
		name    string
		policy  dunning.Policy
		wantErr error
	}{
		{
			name:   "default is valid",
			policy: dunning.DefaultPolicy,
		},
		{
			name:   "custom strictly increasing positive",
			policy: dunning.Policy{WarnDays: 3, OutboundBlockDays: 14, ReadonlyDays: 45, CancelDays: 120},
		},
		{
			name:    "zero warn",
			policy:  dunning.Policy{WarnDays: 0, OutboundBlockDays: 7, ReadonlyDays: 30, CancelDays: 90},
			wantErr: dunning.ErrInvalidPolicy,
		},
		{
			name:    "negative cancel",
			policy:  dunning.Policy{WarnDays: 1, OutboundBlockDays: 7, ReadonlyDays: 30, CancelDays: -1},
			wantErr: dunning.ErrInvalidPolicy,
		},
		{
			name:    "warn equals outbound",
			policy:  dunning.Policy{WarnDays: 7, OutboundBlockDays: 7, ReadonlyDays: 30, CancelDays: 90},
			wantErr: dunning.ErrInvalidPolicy,
		},
		{
			name:    "outbound greater than readonly",
			policy:  dunning.Policy{WarnDays: 1, OutboundBlockDays: 45, ReadonlyDays: 30, CancelDays: 90},
			wantErr: dunning.ErrInvalidPolicy,
		},
		{
			name:    "readonly greater than cancel",
			policy:  dunning.Policy{WarnDays: 1, OutboundBlockDays: 7, ReadonlyDays: 100, CancelDays: 90},
			wantErr: dunning.ErrInvalidPolicy,
		},
		{
			name:    "all zero",
			policy:  dunning.Policy{},
			wantErr: dunning.ErrInvalidPolicy,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got err %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestPolicy_StateForDaysPastDue_Default(t *testing.T) {
	p := dunning.DefaultPolicy

	tests := []struct {
		name string
		days int
		want dunning.State
	}{
		{"not due yet", -5, dunning.StateCurrent},
		{"exactly today", 0, dunning.StateCurrent},
		{"one day before warn", 0, dunning.StateCurrent},
		{"warn boundary inclusive", 1, dunning.StateWarn},
		{"middle of warn window", 5, dunning.StateWarn},
		{"outbound boundary inclusive", 7, dunning.StateSuspendedOutbound},
		{"middle of outbound window", 20, dunning.StateSuspendedOutbound},
		{"readonly boundary inclusive", 30, dunning.StateSuspendedFull},
		{"middle of readonly window", 60, dunning.StateSuspendedFull},
		{"cancel boundary inclusive", 90, dunning.StateCancelled},
		{"well past cancel", 365, dunning.StateCancelled},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.StateForDaysPastDue(tc.days)
			if got != tc.want {
				t.Errorf("StateForDaysPastDue(%d) = %s, want %s", tc.days, got, tc.want)
			}
		})
	}
}

func TestPolicy_StateForDaysPastDue_CustomPlan(t *testing.T) {
	// Enterprise-style plan: 14-day grace, then weekly escalations.
	p := dunning.Policy{
		WarnDays:          14,
		OutboundBlockDays: 21,
		ReadonlyDays:      45,
		CancelDays:        180,
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("custom policy unexpectedly invalid: %v", err)
	}

	tests := []struct {
		days int
		want dunning.State
	}{
		{0, dunning.StateCurrent},
		{13, dunning.StateCurrent},
		{14, dunning.StateWarn},
		{20, dunning.StateWarn},
		{21, dunning.StateSuspendedOutbound},
		{44, dunning.StateSuspendedOutbound},
		{45, dunning.StateSuspendedFull},
		{179, dunning.StateSuspendedFull},
		{180, dunning.StateCancelled},
		{200, dunning.StateCancelled},
	}
	for _, tc := range tests {
		got := p.StateForDaysPastDue(tc.days)
		if got != tc.want {
			t.Errorf("days=%d: got %s, want %s", tc.days, got, tc.want)
		}
	}
}
