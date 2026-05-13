package audit_test

// SIN-62252: contract assertions for the split-audit constants and
// AuditEvent value types. Pure (no DB); the postgres adapter has its
// own integration tests in internal/adapter/db/postgres.

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

func TestSecurityEvent_StableNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		got, want string
	}{
		{string(audit.SecurityEventLogin), "login"},
		{string(audit.SecurityEventLoginFail), "login_fail"},
		{string(audit.SecurityEvent2FAEnroll), "2fa_enroll"},
		{string(audit.SecurityEvent2FAVerify), "2fa_verify"},
		{string(audit.SecurityEventRoleChange), "role_change"},
		{string(audit.SecurityEventImpersonationStart), "impersonation_start"},
		{string(audit.SecurityEventImpersonationStop), "impersonation_stop"},
		{string(audit.SecurityEventMasterGrant), "master_grant"},
		{string(audit.SecurityEventAuthzDeny), "authz_deny"},
		{string(audit.SecurityEventSignatureFail), "signature_fail"},
		{string(audit.SecurityEventKeyRotation), "key_rotation"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("security event constant mismatch: got %q, want %q — wire-stable, do not rename without a migration plan", tc.got, tc.want)
		}
	}
}

func TestDataEvent_StableNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		got, want string
	}{
		{string(audit.DataEventReadPII), "read_pii"},
		{string(audit.DataEventWriteContact), "write_contact"},
		{string(audit.DataEventExportCSV), "export_csv"},
		{string(audit.DataEventLGPDExport), "lgpd_export"},
		{string(audit.DataEventLGPDForget), "lgpd_forget"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("data event constant mismatch: got %q, want %q", tc.got, tc.want)
		}
	}
}

func TestSecurityEvent_IsKnown(t *testing.T) {
	t.Parallel()
	known := []audit.SecurityEvent{
		audit.SecurityEventLogin,
		audit.SecurityEventLoginFail,
		audit.SecurityEvent2FAEnroll,
		audit.SecurityEvent2FAVerify,
		audit.SecurityEventRoleChange,
		audit.SecurityEventImpersonationStart,
		audit.SecurityEventImpersonationStop,
		audit.SecurityEventMasterGrant,
		audit.SecurityEventAuthzDeny,
		audit.SecurityEventSignatureFail,
		audit.SecurityEventKeyRotation,
	}
	for _, e := range known {
		if !e.IsKnown() {
			t.Fatalf("SecurityEvent(%q).IsKnown()=false, want true — adding a constant requires extending allSecurityEvents", e)
		}
	}
	if audit.SecurityEvent("not_a_real_event").IsKnown() {
		t.Fatal("unknown SecurityEvent reported IsKnown=true")
	}
	if audit.SecurityEvent("").IsKnown() {
		t.Fatal("empty SecurityEvent reported IsKnown=true")
	}
}

func TestDataEvent_IsKnown(t *testing.T) {
	t.Parallel()
	known := []audit.DataEvent{
		audit.DataEventReadPII,
		audit.DataEventWriteContact,
		audit.DataEventExportCSV,
		audit.DataEventLGPDExport,
		audit.DataEventLGPDForget,
	}
	for _, e := range known {
		if !e.IsKnown() {
			t.Fatalf("DataEvent(%q).IsKnown()=false, want true", e)
		}
	}
	if audit.DataEvent("not_a_real_event").IsKnown() {
		t.Fatal("unknown DataEvent reported IsKnown=true")
	}
	if audit.DataEvent("").IsKnown() {
		t.Fatal("empty DataEvent reported IsKnown=true")
	}
}

func TestSecurityAuditEvent_ZeroValueIsSafe(t *testing.T) {
	t.Parallel()
	var ev audit.SecurityAuditEvent
	if ev.Event != "" || ev.ActorUserID != uuid.Nil || ev.TenantID != nil || ev.Target != nil || !ev.OccurredAt.IsZero() {
		t.Fatalf("unexpected non-zero fields in zero SecurityAuditEvent: %+v", ev)
	}
	ev.OccurredAt = time.Now().UTC()
	if ev.OccurredAt.IsZero() {
		t.Fatal("expected OccurredAt non-zero after assignment")
	}
}

func TestDataAuditEvent_ZeroValueIsSafe(t *testing.T) {
	t.Parallel()
	var ev audit.DataAuditEvent
	if ev.Event != "" || ev.ActorUserID != uuid.Nil || ev.TenantID != uuid.Nil || ev.Target != nil || !ev.OccurredAt.IsZero() {
		t.Fatalf("unexpected non-zero fields in zero DataAuditEvent: %+v", ev)
	}
}
