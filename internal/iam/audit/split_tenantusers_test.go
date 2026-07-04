package audit_test

import (
	"testing"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

// TestSecurityEvent_TenantUserVocabulary locks the SIN-66496 tenant
// user-management event names (persisted in event_type, mirrored by the
// migration 0136 CHECK — renaming is a breaking change) and asserts each is
// registered in allSecurityEvents so the split writer accepts it. A constant
// declared without the map entry passes every other unit test but is silently
// dropped by WriteSecurity's IsKnown guard at runtime (best-effort, warn-log
// only) — exactly the B1 blocker from the PR #457 CTO review. This test locks
// the guard so it can never regress.
func TestSecurityEvent_TenantUserVocabulary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		event audit.SecurityEvent
		want  string
	}{
		{audit.SecurityEventUserCreate, "user_create"},
		{audit.SecurityEventUserDeactivate, "user_deactivate"},
		{audit.SecurityEventUserReactivate, "user_reactivate"},
		{audit.SecurityEventPasswordReset, "password_reset"},
	}
	for _, tc := range cases {
		if string(tc.event) != tc.want {
			t.Errorf("tenant-user event name = %q, want %q — wire-stable, mirror migration 0136 before renaming", tc.event, tc.want)
		}
		if !tc.event.IsKnown() {
			t.Errorf("SecurityEvent(%q).IsKnown()=false — add it to allSecurityEvents", tc.event)
		}
	}
}
