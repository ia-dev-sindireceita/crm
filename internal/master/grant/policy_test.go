package grant_test

import (
	"testing"

	"github.com/pericles-luz/crm/internal/master/grant"
)

func TestPolicy_Evaluate(t *testing.T) {
	t.Parallel()

	caps := grant.DefaultCaps()
	policyDeny := grant.NewPolicy(caps, false)
	policyApprove := grant.NewPolicy(caps, true)

	req := func(amount int64) grant.Request {
		return grant.Request{
			MasterID:       "m1",
			TenantID:       "t1",
			SubscriptionID: "s1",
			Amount:         amount,
			Reason:         "ok",
		}
	}

	tests := []struct {
		name           string
		policy         grant.MasterGrantPolicy
		req            grant.Request
		subSum         int64
		masterSum      int64
		wantStatus     grant.Status
		wantSubBreach  bool
		wantMasterBrch bool
		wantAlert      bool
	}{
		{
			name:       "small grant under threshold no alert",
			policy:     policyDeny,
			req:        req(500_000),
			wantStatus: grant.StatusGranted,
		},
		{
			name:       "grant just above alert threshold",
			policy:     policyDeny,
			req:        req(1_000_001),
			wantStatus: grant.StatusGranted,
			wantAlert:  true,
		},
		{
			name:          "subscription cap breach denies when approval disabled",
			policy:        policyDeny,
			req:           req(11_000_000),
			wantStatus:    grant.StatusDeniedCapExceeded,
			wantSubBreach: true,
			wantAlert:     true,
		},
		{
			name:          "subscription cap breach pends when approval enabled",
			policy:        policyApprove,
			req:           req(11_000_000),
			wantStatus:    grant.StatusPendingApproval,
			wantSubBreach: true,
			wantAlert:     true,
		},
		{
			name:           "master cap breach via accumulation",
			policy:         policyDeny,
			req:            req(10_000_000),
			subSum:         0,
			masterSum:      100_000_000,
			wantStatus:     grant.StatusDeniedCapExceeded,
			wantMasterBrch: true,
			wantAlert:      true,
		},
		{
			name:           "both caps breached together",
			policy:         policyDeny,
			req:            req(11_000_000),
			masterSum:      100_000_000,
			wantStatus:     grant.StatusDeniedCapExceeded,
			wantSubBreach:  true,
			wantMasterBrch: true,
			wantAlert:      true,
		},
		{
			name:       "grant exactly at subscription cap is allowed",
			policy:     policyDeny,
			req:        req(10_000_000),
			wantStatus: grant.StatusGranted,
			wantAlert:  true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.policy.Evaluate(tc.req, tc.subSum, tc.masterSum)
			if got.Status != tc.wantStatus {
				t.Fatalf("status: want %q, got %q", tc.wantStatus, got.Status)
			}
			if got.Breach.Subscription != tc.wantSubBreach {
				t.Errorf("subscription breach: want %v, got %v", tc.wantSubBreach, got.Breach.Subscription)
			}
			if got.Breach.Master != tc.wantMasterBrch {
				t.Errorf("master breach: want %v, got %v", tc.wantMasterBrch, got.Breach.Master)
			}
			if got.AlertWorthy != tc.wantAlert {
				t.Errorf("alert worthy: want %v, got %v", tc.wantAlert, got.AlertWorthy)
			}
			reasons := got.Breach.Reasons()
			if got.Breach.Any() && len(reasons) == 0 {
				t.Errorf("reasons empty for breach: %#v", got.Breach)
			}
			if !got.Breach.Any() && len(reasons) != 0 {
				t.Errorf("reasons present for no breach: %v", reasons)
			}
		})
	}
}

func TestPolicy_AccessorsAndCaps(t *testing.T) {
	t.Parallel()

	custom := grant.Caps{
		SubscriptionCap90Days: 1,
		MasterCap365Days:      2,
		AlertThreshold:        3,
	}
	p := grant.NewPolicy(custom, true)
	if got := p.Caps(); got != custom {
		t.Errorf("caps: want %+v, got %+v", custom, got)
	}
	if !p.ApprovalEnabled() {
		t.Errorf("approval enabled mismatch")
	}
	if got := grant.DefaultCaps(); got.SubscriptionCap90Days != grant.DefaultSubscriptionCap90Days {
		t.Errorf("default caps mismatch: %+v", got)
	}
}

func TestRequest_Validate(t *testing.T) {
	t.Parallel()

	base := grant.Request{
		MasterID:       "m",
		TenantID:       "t",
		SubscriptionID: "s",
		Amount:         100,
		Reason:         "r",
	}

	cases := []struct {
		name    string
		mutate  func(r *grant.Request)
		wantErr error
	}{
		{"ok", func(*grant.Request) {}, nil},
		{"missing master", func(r *grant.Request) { r.MasterID = " " }, grant.ErrInvalidMaster},
		{"missing tenant", func(r *grant.Request) { r.TenantID = "" }, grant.ErrInvalidTenant},
		{"missing subscription", func(r *grant.Request) { r.SubscriptionID = "" }, grant.ErrInvalidSubscription},
		{"zero amount", func(r *grant.Request) { r.Amount = 0 }, grant.ErrInvalidAmount},
		{"negative amount", func(r *grant.Request) { r.Amount = -1 }, grant.ErrInvalidAmount},
		{"empty reason", func(r *grant.Request) { r.Reason = "  " }, grant.ErrInvalidReason},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := base
			tc.mutate(&r)
			err := r.Validate()
			if err != tc.wantErr {
				t.Errorf("validate: want %v, got %v", tc.wantErr, err)
			}
		})
	}
}
