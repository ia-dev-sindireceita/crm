package iam

import "testing"

func TestRole_Valid(t *testing.T) {
	cases := []struct {
		role Role
		want bool
	}{
		{RoleMaster, true},
		{RoleTenantGerente, true},
		{RoleTenantAtendente, true},
		{RoleTenantCommon, true},
		{Role(""), false},
		{Role("admin"), false},
		{Role("MASTER"), false}, // case-sensitive on purpose
	}
	for _, tc := range cases {
		t.Run(string(tc.role)+"/"+boolName(tc.want), func(t *testing.T) {
			if got := tc.role.Valid(); got != tc.want {
				t.Fatalf("Valid(%q) = %v, want %v", tc.role, got, tc.want)
			}
		})
	}
}

func TestRole_IsMaster(t *testing.T) {
	if !RoleMaster.IsMaster() {
		t.Fatalf("RoleMaster.IsMaster should be true")
	}
	for _, r := range []Role{RoleTenantGerente, RoleTenantAtendente, RoleTenantCommon, Role("")} {
		if r.IsMaster() {
			t.Fatalf("%q.IsMaster should be false", r)
		}
	}
}

func boolName(b bool) string {
	if b {
		return "valid"
	}
	return "invalid"
}
