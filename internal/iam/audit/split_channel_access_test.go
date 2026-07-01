package audit_test

import (
	"testing"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

// TestSecurityEvent_ChannelAccessVocabulary locks the SIN-66405 channel
// access-change event names (they are persisted in event_type and mirrored
// by the migration 0129 CHECK — renaming is a breaking change) and asserts
// each is registered in allSecurityEvents so the split writer will accept
// it. A constant added without the map entry would fail WriteSecurity's
// IsKnown guard at runtime; this test catches that at build time.
func TestSecurityEvent_ChannelAccessVocabulary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		event audit.SecurityEvent
		want  string
	}{
		{audit.SecurityEventChannelAccessGranted, "channel.access_granted"},
		{audit.SecurityEventChannelAccessRevoked, "channel.access_revoked"},
		{audit.SecurityEventChannelRestrictedChanged, "channel.restricted_changed"},
	}
	for _, tc := range cases {
		if string(tc.event) != tc.want {
			t.Errorf("channel access event name = %q, want %q — wire-stable, mirror migration 0129 before renaming", tc.event, tc.want)
		}
		if !tc.event.IsKnown() {
			t.Errorf("SecurityEvent(%q).IsKnown()=false — add it to allSecurityEvents", tc.event)
		}
	}
}
