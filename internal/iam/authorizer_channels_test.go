package iam_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
)

// TestRBAC_ChannelsManage locks the SIN-66391 admin gate: only gerente
// may manage the tenant's channel instances + per-channel access roster
// (managing inbound addressing and who sees which conversations is a
// tenant-admin decision). Atendente, common and a non-impersonating
// master are denied at the gate.
func TestRBAC_ChannelsManage(t *testing.T) {
	t.Parallel()

	authz := iam.NewRBACAuthorizer(iam.RBACConfig{})
	tenant := uuid.New()

	tests := []struct {
		name string
		role iam.Role
		want bool
		code iam.ReasonCode
	}{
		{"gerente-ALLOW", iam.RoleTenantGerente, true, iam.ReasonAllowedRBAC},
		{"atendente-DENY", iam.RoleTenantAtendente, false, iam.ReasonDeniedRBAC},
		{"common-DENY", iam.RoleTenantCommon, false, iam.ReasonDeniedRBAC},
		{"lider-DENY", iam.RoleTenantLider, false, iam.ReasonDeniedRBAC},
		{"master-DENY", iam.RoleMaster, false, iam.ReasonDeniedRBAC},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := iam.Principal{UserID: uuid.New(), TenantID: tenant, Roles: []iam.Role{tc.role}}
			d := authz.Can(context.Background(), p, iam.ActionTenantChannelsManage,
				iam.Resource{TenantID: tenant.String()})
			if d.Allow != tc.want {
				t.Fatalf("Can(%s) Allow = %v, want %v", tc.role, d.Allow, tc.want)
			}
			if d.ReasonCode != tc.code {
				t.Fatalf("Can(%s) ReasonCode = %q, want %q", tc.role, d.ReasonCode, tc.code)
			}
		})
	}
}
