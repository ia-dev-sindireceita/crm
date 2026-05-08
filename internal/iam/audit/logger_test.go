package audit_test

// SIN-62219: contract assertions for the audit Logger port. The
// constants are public API — adapters and downstream consumers grep
// for them — so changing a literal value is a breaking change worth
// catching in CI.

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

func TestEventConstants_StableNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		got, want string
	}{
		{audit.EventImpersonationStarted, "impersonation_started"},
		{audit.EventImpersonationEnded, "impersonation_ended"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("event constant mismatch: got %q, want %q — these are wire-stable strings persisted in audit_log; do not rename without a migration plan", tc.got, tc.want)
		}
	}
}

func TestAuditEvent_ZeroValueIsSafe(t *testing.T) {
	t.Parallel()
	var ev audit.AuditEvent
	if ev.Event != "" || ev.ActorUserID != uuid.Nil || ev.TenantID != nil || ev.Target != nil || !ev.CreatedAt.IsZero() {
		t.Fatalf("unexpected non-zero fields in zero AuditEvent: %+v", ev)
	}
	// Adapter contract: a zero CreatedAt means "let the column DEFAULT
	// fire (server-side now())". Tests pin time.Now via the field.
	ev.CreatedAt = time.Now().UTC()
	if ev.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt non-zero after assignment")
	}
}
