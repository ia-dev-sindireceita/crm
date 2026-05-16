package iam_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
)

// fixedNow returns a deterministic now function for the Authorizer.
func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// principalFor builds a Principal for tests. Master callers pass
// RoleMaster; tenant callers pass one of the tenant roles. The userID
// is non-nil so the "no principal" guard does not fire.
func principalFor(t *testing.T, role iam.Role, masterImp bool, mfaAt *time.Time) iam.Principal {
	t.Helper()
	return iam.Principal{
		UserID:              uuid.New(),
		TenantID:            uuid.New(),
		Roles:               []iam.Role{role},
		MasterImpersonating: masterImp,
		MFAVerifiedAt:       mfaAt,
	}
}

// TestRBACAuthorizer_ContractMatrix is the role × action contract
// table-driven test (ADR 0090 §RBAC matrix). Every cell here is
// load-bearing: when the matrix changes, the test changes alongside.
func TestRBACAuthorizer_ContractMatrix(t *testing.T) {
	t.Parallel()
	authz := iam.NewRBACAuthorizer(iam.RBACConfig{})

	tests := []struct {
		name       string
		role       iam.Role
		action     iam.Action
		wantAllow  bool
		wantReason iam.ReasonCode
	}{
		// Tenant common — read-only basic.
		{"common-read-contact", iam.RoleTenantCommon, iam.ActionTenantContactRead, true, iam.ReasonAllowedRBAC},
		{"common-read-pii-DENY", iam.RoleTenantCommon, iam.ActionTenantContactReadPII, false, iam.ReasonDeniedRBAC},
		{"common-update-contact-DENY", iam.RoleTenantCommon, iam.ActionTenantContactUpdate, false, iam.ReasonDeniedRBAC},
		{"common-send-message-DENY", iam.RoleTenantCommon, iam.ActionTenantMessageSend, false, iam.ReasonDeniedRBAC},
		{"common-read-conversation", iam.RoleTenantCommon, iam.ActionTenantConversationRead, true, iam.ReasonAllowedRBAC},

		// Tenant atendente — read + send + update contacts.
		{"atendente-update-contact", iam.RoleTenantAtendente, iam.ActionTenantContactUpdate, true, iam.ReasonAllowedRBAC},
		{"atendente-send-message", iam.RoleTenantAtendente, iam.ActionTenantMessageSend, true, iam.ReasonAllowedRBAC},
		{"atendente-create-contact-DENY", iam.RoleTenantAtendente, iam.ActionTenantContactCreate, false, iam.ReasonDeniedRBAC},
		{"atendente-delete-contact-DENY", iam.RoleTenantAtendente, iam.ActionTenantContactDelete, false, iam.ReasonDeniedRBAC},
		{"atendente-read-pii-DENY", iam.RoleTenantAtendente, iam.ActionTenantContactReadPII, false, iam.ReasonDeniedRBAC},

		// Tenant gerente — full tenant set including PII.
		{"gerente-create-contact", iam.RoleTenantGerente, iam.ActionTenantContactCreate, true, iam.ReasonAllowedRBAC},
		{"gerente-delete-contact", iam.RoleTenantGerente, iam.ActionTenantContactDelete, true, iam.ReasonAllowedRBAC},
		{"gerente-read-pii", iam.RoleTenantGerente, iam.ActionTenantContactReadPII, true, iam.ReasonAllowedRBAC},
		{"gerente-send-message", iam.RoleTenantGerente, iam.ActionTenantMessageSend, true, iam.ReasonAllowedRBAC},

		// Master — tenant.* actions are NOT allowed without impersonation.
		{"master-tenant-read-DENY", iam.RoleMaster, iam.ActionTenantContactRead, false, iam.ReasonDeniedRBAC},
		{"master-tenant-pii-DENY", iam.RoleMaster, iam.ActionTenantContactReadPII, false, iam.ReasonDeniedRBAC},

		// Master — master.* actions allowed.
		{"master-tenant-create", iam.RoleMaster, iam.ActionMasterTenantCreate, true, iam.ReasonAllowedMaster},
		{"master-tenant-impersonate", iam.RoleMaster, iam.ActionMasterTenantImpersonate, true, iam.ReasonAllowedMaster},

		// Non-master attempting master.* — denied.
		{"gerente-master-create-DENY", iam.RoleTenantGerente, iam.ActionMasterTenantCreate, false, iam.ReasonDeniedRBAC},

		// SIN-62880 — master.subscription.* allowed only for master.
		{"master-subscription-assign-plan-ALLOW", iam.RoleMaster, iam.ActionMasterSubscriptionAssignPlan, true, iam.ReasonAllowedMaster},
		{"master-subscription-cancel-ALLOW", iam.RoleMaster, iam.ActionMasterSubscriptionCancel, true, iam.ReasonAllowedMaster},
		{"gerente-subscription-assign-plan-DENY", iam.RoleTenantGerente, iam.ActionMasterSubscriptionAssignPlan, false, iam.ReasonDeniedRBAC},
		{"gerente-subscription-cancel-DENY", iam.RoleTenantGerente, iam.ActionMasterSubscriptionCancel, false, iam.ReasonDeniedRBAC},
		{"atendente-subscription-assign-plan-DENY", iam.RoleTenantAtendente, iam.ActionMasterSubscriptionAssignPlan, false, iam.ReasonDeniedRBAC},
		{"common-subscription-cancel-DENY", iam.RoleTenantCommon, iam.ActionMasterSubscriptionCancel, false, iam.ReasonDeniedRBAC},

		// SIN-62880 — master.grant_courtesy.* allowed only for master.
		{"master-grant-free-period-ALLOW", iam.RoleMaster, iam.ActionMasterGrantCourtesyFreeSubscriptionPeriod, true, iam.ReasonAllowedMaster},
		{"master-grant-extra-tokens-ALLOW", iam.RoleMaster, iam.ActionMasterGrantCourtesyExtraTokens, true, iam.ReasonAllowedMaster},
		{"master-grant-revoke-ALLOW", iam.RoleMaster, iam.ActionMasterGrantCourtesyRevoke, true, iam.ReasonAllowedMaster},
		{"gerente-grant-free-period-DENY", iam.RoleTenantGerente, iam.ActionMasterGrantCourtesyFreeSubscriptionPeriod, false, iam.ReasonDeniedRBAC},
		{"gerente-grant-extra-tokens-DENY", iam.RoleTenantGerente, iam.ActionMasterGrantCourtesyExtraTokens, false, iam.ReasonDeniedRBAC},
		{"gerente-grant-revoke-DENY", iam.RoleTenantGerente, iam.ActionMasterGrantCourtesyRevoke, false, iam.ReasonDeniedRBAC},
		{"atendente-grant-extra-tokens-DENY", iam.RoleTenantAtendente, iam.ActionMasterGrantCourtesyExtraTokens, false, iam.ReasonDeniedRBAC},
		{"common-grant-revoke-DENY", iam.RoleTenantCommon, iam.ActionMasterGrantCourtesyRevoke, false, iam.ReasonDeniedRBAC},

		// SIN-62880 — tenant.billing.view + tenant.wallet.view_ledger
		// allowed only for tenant_gerente; atendente, common, and master
		// (without impersonation) are denied.
		{"gerente-billing-view-ALLOW", iam.RoleTenantGerente, iam.ActionTenantBillingView, true, iam.ReasonAllowedRBAC},
		{"gerente-wallet-view-ledger-ALLOW", iam.RoleTenantGerente, iam.ActionTenantWalletViewLedger, true, iam.ReasonAllowedRBAC},
		{"atendente-billing-view-DENY", iam.RoleTenantAtendente, iam.ActionTenantBillingView, false, iam.ReasonDeniedRBAC},
		{"atendente-wallet-view-ledger-DENY", iam.RoleTenantAtendente, iam.ActionTenantWalletViewLedger, false, iam.ReasonDeniedRBAC},
		{"common-billing-view-DENY", iam.RoleTenantCommon, iam.ActionTenantBillingView, false, iam.ReasonDeniedRBAC},
		{"common-wallet-view-ledger-DENY", iam.RoleTenantCommon, iam.ActionTenantWalletViewLedger, false, iam.ReasonDeniedRBAC},
		{"master-billing-view-DENY", iam.RoleMaster, iam.ActionTenantBillingView, false, iam.ReasonDeniedRBAC},
		{"master-wallet-view-ledger-DENY", iam.RoleMaster, iam.ActionTenantWalletViewLedger, false, iam.ReasonDeniedRBAC},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := principalFor(t, tc.role, false, nil)
			d := authz.Can(context.Background(), p, tc.action, iam.Resource{Kind: "contact", ID: "fixture"})
			if d.Allow != tc.wantAllow {
				t.Fatalf("Allow = %v, want %v (reason %q)", d.Allow, tc.wantAllow, d.ReasonCode)
			}
			if d.ReasonCode != tc.wantReason {
				t.Fatalf("ReasonCode = %q, want %q", d.ReasonCode, tc.wantReason)
			}
			if d.TargetKind != "contact" || d.TargetID != "fixture" {
				t.Fatalf("target echo wrong: kind=%q id=%q", d.TargetKind, d.TargetID)
			}
		})
	}
}

// TestRBACAuthorizer_UnknownAction asserts the deny-by-default
// behaviour for an action that is not in the matrix.
func TestRBACAuthorizer_UnknownAction(t *testing.T) {
	t.Parallel()
	authz := iam.NewRBACAuthorizer(iam.RBACConfig{})
	p := principalFor(t, iam.RoleTenantGerente, false, nil)
	d := authz.Can(context.Background(), p, iam.Action("tenant.totally.invented"), iam.Resource{})
	if d.Allow {
		t.Fatal("unknown action must deny")
	}
	if d.ReasonCode != iam.ReasonDeniedUnknownAction {
		t.Fatalf("reason = %q, want %q", d.ReasonCode, iam.ReasonDeniedUnknownAction)
	}
}

// TestRBACAuthorizer_EmptyPrincipal asserts the guard against an
// unauthenticated caller leaking past RequireAuth.
func TestRBACAuthorizer_EmptyPrincipal(t *testing.T) {
	t.Parallel()
	authz := iam.NewRBACAuthorizer(iam.RBACConfig{})
	p := iam.Principal{TenantID: uuid.New(), Roles: []iam.Role{iam.RoleTenantGerente}}
	d := authz.Can(context.Background(), p, iam.ActionTenantContactRead, iam.Resource{})
	if d.Allow {
		t.Fatal("empty UserID must deny")
	}
	if d.ReasonCode != iam.ReasonDeniedNoPrincipal {
		t.Fatalf("reason = %q, want %q", d.ReasonCode, iam.ReasonDeniedNoPrincipal)
	}
}

// TestRBACAuthorizer_TenantMismatch asserts the per-resource tenant
// scoping: a Resource.TenantID different from Principal.TenantID is
// denied even when the role would otherwise permit the action.
func TestRBACAuthorizer_TenantMismatch(t *testing.T) {
	t.Parallel()
	authz := iam.NewRBACAuthorizer(iam.RBACConfig{})
	p := principalFor(t, iam.RoleTenantGerente, false, nil)
	other := uuid.New().String()
	d := authz.Can(context.Background(), p, iam.ActionTenantContactRead, iam.Resource{TenantID: other})
	if d.Allow {
		t.Fatal("cross-tenant resource must deny")
	}
	if d.ReasonCode != iam.ReasonDeniedTenantMismatch {
		t.Fatalf("reason = %q, want %q", d.ReasonCode, iam.ReasonDeniedTenantMismatch)
	}
}

// TestRBACAuthorizer_MasterPIIStepUp covers the ADR 0090 §M3 gate:
// a master impersonating a tenant attempting a PII read is denied
// unless MFAVerifiedAt is within MasterPIIStepUpWindow of Now.
func TestRBACAuthorizer_MasterPIIStepUp(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	window := 5 * time.Minute

	tests := []struct {
		name      string
		mfaAt     *time.Time
		wantAllow bool
		want      iam.ReasonCode
	}{
		{
			name:      "no-step-up-DENY",
			mfaAt:     nil,
			wantAllow: false,
			want:      iam.ReasonDeniedMasterPIIStepUp,
		},
		{
			name:      "stale-step-up-DENY",
			mfaAt:     ptr(now.Add(-6 * time.Minute)),
			wantAllow: false,
			want:      iam.ReasonDeniedMasterPIIStepUp,
		},
		{
			name:      "fresh-step-up-ALLOW",
			mfaAt:     ptr(now.Add(-1 * time.Minute)),
			wantAllow: true,
			want:      iam.ReasonAllowedRBAC,
		},
		{
			name:      "edge-window-ALLOW",
			mfaAt:     ptr(now.Add(-5 * time.Minute)),
			wantAllow: true,
			want:      iam.ReasonAllowedRBAC,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			authz := iam.NewRBACAuthorizer(iam.RBACConfig{
				MasterPIIStepUpWindow: window,
				Now:                   fixedNow(now),
			})
			// Master impersonating a tenant — Roles carries the
			// effective tenant role plus master flag. For the gate
			// to fire we need a role that WOULD allow the action.
			p := iam.Principal{
				UserID:              uuid.New(),
				TenantID:            uuid.New(),
				Roles:               []iam.Role{iam.RoleTenantGerente},
				MasterImpersonating: true,
				MFAVerifiedAt:       tc.mfaAt,
			}
			d := authz.Can(context.Background(), p, iam.ActionTenantContactReadPII, iam.Resource{Kind: "contact", ID: "c1"})
			if d.Allow != tc.wantAllow {
				t.Fatalf("Allow = %v, want %v (reason %q)", d.Allow, tc.wantAllow, d.ReasonCode)
			}
			if d.ReasonCode != tc.want {
				t.Fatalf("Reason = %q, want %q", d.ReasonCode, tc.want)
			}
		})
	}
}

// TestRBACAuthorizer_MasterPIIStepUp_NonPII asserts the gate does NOT
// fire for a non-PII tenant action while master is impersonating —
// only PII reads require fresh step-up.
func TestRBACAuthorizer_MasterPIIStepUp_NonPII(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	authz := iam.NewRBACAuthorizer(iam.RBACConfig{
		MasterPIIStepUpWindow: 5 * time.Minute,
		Now:                   fixedNow(now),
	})
	p := iam.Principal{
		UserID:              uuid.New(),
		TenantID:            uuid.New(),
		Roles:               []iam.Role{iam.RoleTenantCommon},
		MasterImpersonating: true,
		MFAVerifiedAt:       nil, // stale on purpose
	}
	d := authz.Can(context.Background(), p, iam.ActionTenantContactRead, iam.Resource{})
	if !d.Allow {
		t.Fatalf("non-PII action must not trigger PII gate; got reason %q", d.ReasonCode)
	}
	if d.ReasonCode != iam.ReasonAllowedRBAC {
		t.Fatalf("reason = %q, want %q", d.ReasonCode, iam.ReasonAllowedRBAC)
	}
}

// TestRBACAuthorizer_MasterPIIStepUp_ZeroWindow exercises the
// edge case where the configured window is zero: any step-up in the
// past is stale, but a step-up exactly at Now is fresh.
func TestRBACAuthorizer_MasterPIIStepUp_ZeroWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	authz := iam.NewRBACAuthorizer(iam.RBACConfig{
		MasterPIIStepUpWindow: 0,
		Now:                   fixedNow(now),
	})

	p := iam.Principal{
		UserID:              uuid.New(),
		TenantID:            uuid.New(),
		Roles:               []iam.Role{iam.RoleTenantGerente},
		MasterImpersonating: true,
		MFAVerifiedAt:       ptr(now),
	}
	d := authz.Can(context.Background(), p, iam.ActionTenantContactReadPII, iam.Resource{})
	if !d.Allow {
		t.Fatalf("step-up at Now with zero window must allow; got %q", d.ReasonCode)
	}

	p.MFAVerifiedAt = ptr(now.Add(-1 * time.Nanosecond))
	d = authz.Can(context.Background(), p, iam.ActionTenantContactReadPII, iam.Resource{})
	if d.Allow {
		t.Fatalf("step-up before Now with zero window must deny; got allow")
	}
	if d.ReasonCode != iam.ReasonDeniedMasterPIIStepUp {
		t.Fatalf("reason = %q, want %q", d.ReasonCode, iam.ReasonDeniedMasterPIIStepUp)
	}
}

// TestPrincipal_ContextRoundTrip asserts the context plumbing.
func TestPrincipal_ContextRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, ok := iam.PrincipalFromContext(ctx); ok {
		t.Fatal("empty context must not yield a Principal")
	}
	want := iam.Principal{UserID: uuid.New(), TenantID: uuid.New(), Roles: []iam.Role{iam.RoleTenantGerente}}
	got, ok := iam.PrincipalFromContext(iam.WithPrincipal(ctx, want))
	if !ok {
		t.Fatal("WithPrincipal then FromContext must yield ok=true")
	}
	if got.UserID != want.UserID || got.TenantID != want.TenantID {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, want)
	}
}

// TestPrincipalFromSession smoke-tests the Session adapter helper.
func TestPrincipalFromSession(t *testing.T) {
	t.Parallel()
	s := iam.Session{UserID: uuid.New(), TenantID: uuid.New(), Role: iam.RoleTenantAtendente}
	mfa := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	p := iam.PrincipalFromSession(s, true, &mfa)
	if p.UserID != s.UserID || p.TenantID != s.TenantID {
		t.Fatal("uuid copy failed")
	}
	if !p.HasRole(iam.RoleTenantAtendente) {
		t.Fatalf("role not propagated: %+v", p.Roles)
	}
	if !p.MasterImpersonating {
		t.Fatal("MasterImpersonating not propagated")
	}
	if p.MFAVerifiedAt == nil || !p.MFAVerifiedAt.Equal(mfa) {
		t.Fatalf("MFAVerifiedAt mismatch: %v", p.MFAVerifiedAt)
	}
	if !p.HasRole(iam.RoleTenantAtendente) {
		t.Fatal("HasRole did not match an added role")
	}
	if p.IsMaster() {
		t.Fatal("IsMaster should be false without RoleMaster")
	}
}

func ptr[T any](v T) *T { return &v }
