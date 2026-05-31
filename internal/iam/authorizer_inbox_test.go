package iam_test

// SIN-63821 (parent SIN-63793) — ActionTenantInboxRead matrix tests.
//
// The contract for the inbox surface, locked here so a future matrix
// edit cannot silently flip the gate. The numbers in the table are the
// AC for SIN-63821:
//
//   - tenant_atendente → 200 (allow)
//   - tenant_gerente   → 200 (allow, role-superset)
//   - tenant_common    → 403 (deny — CEO ACK 2026-05-31 on SIN-63808)
//   - master (no impersonation) → 403 (tenant-scope action)
//   - empty principal  → 403 (RequireAuth normally intercepts; defense
//     in depth)
//
// Adding cases to the existing TestRBACAuthorizer_ContractMatrix table
// would modify a read-only test (Quality bar Rule 3), so this file is
// purely additive — same package, fresh test names, no edits upstream.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
)

func TestRBACAuthorizer_InboxRead_Matrix(t *testing.T) {
	t.Parallel()
	authz := iam.NewRBACAuthorizer(iam.RBACConfig{})

	cases := []struct {
		name       string
		role       iam.Role
		wantAllow  bool
		wantReason iam.ReasonCode
	}{
		{"atendente-allow", iam.RoleTenantAtendente, true, iam.ReasonAllowedRBAC},
		{"gerente-allow", iam.RoleTenantGerente, true, iam.ReasonAllowedRBAC},
		{"common-deny", iam.RoleTenantCommon, false, iam.ReasonDeniedRBAC},
		{"master-deny-without-impersonation", iam.RoleMaster, false, iam.ReasonDeniedRBAC},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := iam.Principal{
				UserID:   uuid.New(),
				TenantID: uuid.New(),
				Roles:    []iam.Role{tc.role},
			}
			d := authz.Can(context.Background(), p, iam.ActionTenantInboxRead, iam.Resource{
				TenantID: p.TenantID.String(),
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

// TestRBACAuthorizer_InboxRead_EmptyPrincipalDenied locks the defense-
// in-depth guard: an empty Principal (UUID zero) reaches RequireAction
// only when RequireAuth was bypassed (programmer error). The authorizer
// MUST still deny with ReasonDeniedNoPrincipal rather than fall through
// to RBAC. Verifies the matrix entry exists (otherwise the
// ReasonDeniedUnknownAction branch fires instead).
func TestRBACAuthorizer_InboxRead_EmptyPrincipalDenied(t *testing.T) {
	t.Parallel()
	authz := iam.NewRBACAuthorizer(iam.RBACConfig{})

	d := authz.Can(context.Background(), iam.Principal{}, iam.ActionTenantInboxRead, iam.Resource{})
	if d.Allow {
		t.Fatalf("Allow = true on empty Principal, want false (decision=%+v)", d)
	}
	if d.ReasonCode != iam.ReasonDeniedNoPrincipal {
		t.Fatalf("ReasonCode = %q, want %q", d.ReasonCode, iam.ReasonDeniedNoPrincipal)
	}
}

// TestRBACAuthorizer_InboxRead_TenantMismatchDenied locks the tenant-
// boundary check: an atendente whose Principal.TenantID does not match
// the Resource.TenantID is denied even though the role-allow set
// matches. Protects against a future routing bug that lets a session
// from tenant A read /inbox under tenant B's host.
func TestRBACAuthorizer_InboxRead_TenantMismatchDenied(t *testing.T) {
	t.Parallel()
	authz := iam.NewRBACAuthorizer(iam.RBACConfig{})

	p := iam.Principal{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []iam.Role{iam.RoleTenantAtendente},
	}
	d := authz.Can(context.Background(), p, iam.ActionTenantInboxRead, iam.Resource{
		TenantID: uuid.New().String(), // different tenant
		Kind:     "inbox",
	})
	if d.Allow {
		t.Fatalf("Allow = true on tenant mismatch, want false (decision=%+v)", d)
	}
	if d.ReasonCode != iam.ReasonDeniedTenantMismatch {
		t.Fatalf("ReasonCode = %q, want %q", d.ReasonCode, iam.ReasonDeniedTenantMismatch)
	}
}
