package audit_test

// SIN-62254: assertions for the SecurityEventAuthzAllow constant added
// alongside the authz wrapper. The existing exhaustiveness tests in
// split_test.go are read-only (CTO rule 3); this file adds the new
// constant's coverage without touching them.

import (
	"testing"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

func TestSecurityEventAuthzAllow_StableName(t *testing.T) {
	t.Parallel()
	if got, want := string(audit.SecurityEventAuthzAllow), "authz_allow"; got != want {
		t.Fatalf("authz_allow constant drifted: got %q, want %q", got, want)
	}
}

func TestSecurityEventAuthzAllow_IsKnown(t *testing.T) {
	t.Parallel()
	if !audit.SecurityEventAuthzAllow.IsKnown() {
		t.Fatal("SecurityEventAuthzAllow.IsKnown()=false — extend allSecurityEvents in split.go")
	}
}
