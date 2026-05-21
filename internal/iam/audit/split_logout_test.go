package audit_test

// SIN-63188 / Fase 6 PR6 contract assertions for SecurityEventLogout.
// Kept in a sibling file so the established split_test.go enumeration
// stays untouched (project policy: existing tests are read-only without
// CTO authorization). Mirrors the IsKnown / wire-stable checks the
// established suite applies to every other event constant.

import (
	"testing"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

func TestSecurityEventLogout_WireName(t *testing.T) {
	t.Parallel()
	if got, want := string(audit.SecurityEventLogout), "logout"; got != want {
		t.Fatalf("SecurityEventLogout constant: got %q want %q — wire-stable, do not rename without a migration plan", got, want)
	}
}

func TestSecurityEventLogout_IsKnown(t *testing.T) {
	t.Parallel()
	if !audit.SecurityEventLogout.IsKnown() {
		t.Fatal("SecurityEventLogout.IsKnown()=false — extend allSecurityEvents in split.go")
	}
}
