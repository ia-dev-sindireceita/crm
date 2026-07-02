package iam_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
)

// TestRBAC_TenantUserManage locks the SIN-66499 admin gate: only gerente
// may list / create / update / deactivate the tenant's users (managing who
// logs into the tenant is a tenant-admin decision). Atendente, common,
// lider and a non-impersonating master are denied at every verb.
func TestRBAC_TenantUserManage(t *testing.T) {
	t.Parallel()

	authz := iam.NewRBACAuthorizer(iam.RBACConfig{})
	tenant := uuid.New()

	actions := []iam.Action{
		iam.ActionTenantUserList,
		iam.ActionTenantUserCreate,
		iam.ActionTenantUserUpdate,
		iam.ActionTenantUserDeactivate,
	}
	roles := []struct {
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

	for _, action := range actions {
		for _, tc := range roles {
			name := string(action) + "/" + tc.name
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				p := iam.Principal{UserID: uuid.New(), TenantID: tenant, Roles: []iam.Role{tc.role}}
				d := authz.Can(context.Background(), p, action, iam.Resource{TenantID: tenant.String()})
				if d.Allow != tc.want {
					t.Fatalf("Can(%s, %s) Allow = %v, want %v", action, tc.role, d.Allow, tc.want)
				}
				if d.ReasonCode != tc.code {
					t.Fatalf("Can(%s, %s) ReasonCode = %q, want %q", action, tc.role, d.ReasonCode, tc.code)
				}
			})
		}
	}
}

// TestRBAC_TenantUser_CrossTenantMismatch locks the tenant-isolation gate:
// a gerente of tenant A is denied when the resource names tenant B, even
// though gerente is the allowed role. This is the application-layer link of
// the isolation defense-in-depth (RLS is the storage-layer link).
func TestRBAC_TenantUser_CrossTenantMismatch(t *testing.T) {
	t.Parallel()
	authz := iam.NewRBACAuthorizer(iam.RBACConfig{})
	tenantA := uuid.New()
	tenantB := uuid.New()
	p := iam.Principal{UserID: uuid.New(), TenantID: tenantA, Roles: []iam.Role{iam.RoleTenantGerente}}
	d := authz.Can(context.Background(), p, iam.ActionTenantUserCreate, iam.Resource{TenantID: tenantB.String()})
	if d.Allow {
		t.Fatalf("gerente of tenant A must not act on tenant B")
	}
	if d.ReasonCode != iam.ReasonDeniedTenantMismatch {
		t.Fatalf("ReasonCode = %q, want %q", d.ReasonCode, iam.ReasonDeniedTenantMismatch)
	}
}
