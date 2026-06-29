package audit_test

// SIN-66305 (R3 / SIN-66292) gate 6 — the two WhatsApp-session transition
// security events are in the controlled vocabulary (added to
// allSecurityEvents) and carry their stable wire literals, mirrored by the
// migration 0127 CHECK clause.

import (
	"testing"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

func TestWASessionSecurityEvents(t *testing.T) {
	t.Parallel()
	cases := map[audit.SecurityEvent]string{
		audit.SecurityEventWASessionBanned:       "wa_session.banned",
		audit.SecurityEventWASessionDisconnected: "wa_session.disconnected",
	}
	for evt, want := range cases {
		if string(evt) != want {
			t.Errorf("wire literal = %q, want %q (must match migration 0127 CHECK)", evt, want)
		}
		if !evt.IsKnown() {
			t.Errorf("SecurityEvent(%q).IsKnown() = false — add it to allSecurityEvents", evt)
		}
	}
}
