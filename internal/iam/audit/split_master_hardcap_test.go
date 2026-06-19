package audit_test

// SIN-65232 (follow-up from SIN-65223 / PR #368) contract assertions for
// SecurityEventMasterSessionHardCapHit. Kept in a sibling file so the
// established split_test.go enumeration stays untouched (project policy:
// existing tests are read-only without CTO authorization). Mirrors the
// IsKnown / wire-stable checks the established suite applies to every
// other event constant.

import (
	"testing"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

func TestSecurityEventMasterSessionHardCapHit_WireName(t *testing.T) {
	t.Parallel()
	if got, want := string(audit.SecurityEventMasterSessionHardCapHit), "master.session.hard_cap_hit"; got != want {
		t.Fatalf("SecurityEventMasterSessionHardCapHit constant: got %q want %q — wire-stable, must match the migration 0122 CHECK literal; do not rename without a migration plan", got, want)
	}
}

func TestSecurityEventMasterSessionHardCapHit_IsKnown(t *testing.T) {
	t.Parallel()
	if !audit.SecurityEventMasterSessionHardCapHit.IsKnown() {
		t.Fatal("SecurityEventMasterSessionHardCapHit.IsKnown()=false — extend allSecurityEvents in split.go")
	}
}
