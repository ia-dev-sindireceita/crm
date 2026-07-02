package iam_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
)

// TestRBACAuthorizer_TenantUserActions pins SIN-66496 / SIN-66494 G3: the
// tenant user-management actions are gerente-only. atendente / common /
// líder are denied (least privilege, deny-by-default). Master is a separate
// plane and is not granted these tenant-scoped actions.
func TestRBACAuthorizer_TenantUserActions(t *testing.T) {
	t.Parallel()
	authz := iam.NewRBACAuthorizer(iam.RBACConfig{})

	actions := []iam.Action{
		iam.ActionTenantUserList,
		iam.ActionTenantUserCreate,
		iam.ActionTenantUserUpdate,
		iam.ActionTenantUserDeactivate,
	}
	cases := []struct {
		role  iam.Role
		allow bool
	}{
		{iam.RoleTenantGerente, true},
		{iam.RoleTenantAtendente, false},
		{iam.RoleTenantCommon, false},
		{iam.RoleTenantLider, false},
		{iam.RoleMaster, false},
	}

	for _, action := range actions {
		for _, c := range cases {
			p := iam.Principal{UserID: uuid.New(), TenantID: uuid.New(), Roles: []iam.Role{c.role}}
			d := authz.Can(context.Background(), p, action, iam.Resource{Kind: "user", ID: "fixture"})
			if d.Allow != c.allow {
				t.Errorf("%s / %s: Allow = %v, want %v (reason %q)", action, c.role, d.Allow, c.allow, d.ReasonCode)
			}
		}
	}
}
