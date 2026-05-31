package iam_test

// SIN-63858 — regression guard for the staging /inbox 403 that hit
// agent@acme.<base_domain> after the SIN-63821 gate landed. The seed
// user shipped as RoleTenantCommon (from SIN-63342); the gate at
// ActionTenantInboxRead is {RoleTenantAtendente, RoleTenantGerente}.
//
// The three subtests below document the floor: pre-fix common is
// denied (`agent_seed_tenant_common_denied_pre_fix`), and post-fix the
// atendente and gerente arms of the gate both allow. The first case is
// the regression proof — it is not a fix-side-effect; it stays green
// before AND after the seed flip in stg.sql + migration 0115, because
// the seed's role string is what changes, not the authorizer matrix.
//
// This file is purely additive — the matrix coverage in
// authorizer_inbox_test.go (SIN-63821) overlaps deliberately but does
// not reference SIN-63858 by name; this file is the documented
// regression entry point a future reader will find via `grep
// SIN-63858` after the staging incident.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
)

func TestRBACAuthorizer_InboxRead_SIN63858_Regression(t *testing.T) {
	t.Parallel()
	authz := iam.NewRBACAuthorizer(iam.RBACConfig{})

	cases := []struct {
		name       string
		role       iam.Role
		wantAllow  bool
		wantReason iam.ReasonCode
	}{
		{
			// Pre-fix shape: agent@<tenant> seeded as tenant_common is
			// denied at the authorizer. This is the bug observed on
			// staging (403 on /inbox). It must stay denied after the
			// seed flip too — atendente is the gate, common is below it.
			name:       "agent_seed_tenant_common_denied_pre_fix",
			role:       iam.RoleTenantCommon,
			wantAllow:  false,
			wantReason: iam.ReasonDeniedRBAC,
		},
		{
			// Post-fix shape: agent@<tenant> reseeded as tenant_atendente
			// passes the gate. This is the case staging is now expected
			// to satisfy via stg.sql + migration 0115.
			name:       "atendente_post_fix_allowed",
			role:       iam.RoleTenantAtendente,
			wantAllow:  true,
			wantReason: iam.ReasonAllowedRBAC,
		},
		{
			// Role-superset arm: gerente keeps inbox access (covers the
			// admin@acme seed row already used by the SIN-63821 probes).
			name:       "gerente_post_fix_allowed",
			role:       iam.RoleTenantGerente,
			wantAllow:  true,
			wantReason: iam.ReasonAllowedRBAC,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tenantID := uuid.New()
			p := iam.Principal{
				UserID:   uuid.New(),
				TenantID: tenantID,
				Roles:    []iam.Role{tc.role},
			}
			d := authz.Can(context.Background(), p, iam.ActionTenantInboxRead, iam.Resource{
				TenantID: tenantID.String(),
				Kind:     "inbox",
			})
			if d.Allow != tc.wantAllow {
				t.Fatalf("Allow = %v, want %v (reason=%q)", d.Allow, tc.wantAllow, d.ReasonCode)
			}
			if d.ReasonCode != tc.wantReason {
				t.Fatalf("ReasonCode = %q, want %q", d.ReasonCode, tc.wantReason)
			}
		})
	}
}
