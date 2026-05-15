package audit

// SIN-62252 / ADR 0004 §4: split-audit constants and port.
//
// The split routes every event into one of two append-only ledgers
// with different retention rules:
//
//   * SecurityEvent — identity / authn / authz / key-management events.
//     Retention: 24 months minimum, never purged by the LGPD job.
//   * DataEvent — PII access and LGPD bookkeeping. Retention is per
//     tenant (`tenants.audit_data_retention_months`, default 12) and
//     the LGPD purge job sweeps this category and only this category.
//
// Constants are wire-stable: they are persisted in the `event_type`
// column and asserted by master-panel queries. Renaming a constant is
// a breaking change that needs a migration plan. Adding a constant
// requires the matching CHECK clause to be extended in migration 0012
// first.

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// SecurityEvent is the controlled vocabulary of audit_log_security
// rows. The list mirrors the CHECK clause of audit_log_security in
// migration 0083_split_audit_log.up.sql.
type SecurityEvent string

const (
	SecurityEventLogin              SecurityEvent = "login"
	SecurityEventLoginFail          SecurityEvent = "login_fail"
	SecurityEvent2FAEnroll          SecurityEvent = "2fa_enroll"
	SecurityEvent2FAVerify          SecurityEvent = "2fa_verify"
	SecurityEventRoleChange         SecurityEvent = "role_change"
	SecurityEventImpersonationStart SecurityEvent = "impersonation_start"
	SecurityEventImpersonationStop  SecurityEvent = "impersonation_stop"
	SecurityEventMasterGrant        SecurityEvent = "master_grant"
	SecurityEventAuthzDeny          SecurityEvent = "authz_deny"
	SecurityEventAuthzAllow         SecurityEvent = "authz_allow"
	SecurityEventSignatureFail      SecurityEvent = "signature_fail"
	SecurityEventKeyRotation        SecurityEvent = "key_rotation"
)

// DataEvent is the controlled vocabulary of audit_log_data rows.
// Mirrors the CHECK clause of audit_log_data in migration 0012.
type DataEvent string

const (
	DataEventReadPII      DataEvent = "read_pii"
	DataEventWriteContact DataEvent = "write_contact"
	DataEventExportCSV    DataEvent = "export_csv"
	DataEventLGPDExport   DataEvent = "lgpd_export"
	DataEventLGPDForget   DataEvent = "lgpd_forget"
)

var allSecurityEvents = map[SecurityEvent]struct{}{
	SecurityEventLogin:              {},
	SecurityEventLoginFail:          {},
	SecurityEvent2FAEnroll:          {},
	SecurityEvent2FAVerify:          {},
	SecurityEventRoleChange:         {},
	SecurityEventImpersonationStart: {},
	SecurityEventImpersonationStop:  {},
	SecurityEventMasterGrant:        {},
	SecurityEventAuthzDeny:          {},
	SecurityEventAuthzAllow:         {},
	SecurityEventSignatureFail:      {},
	SecurityEventKeyRotation:        {},
}

var allDataEvents = map[DataEvent]struct{}{
	DataEventReadPII:      {},
	DataEventWriteContact: {},
	DataEventExportCSV:    {},
	DataEventLGPDExport:   {},
	DataEventLGPDForget:   {},
}

// IsKnown reports whether e is in the controlled vocabulary.
// Adapters can call IsKnown before a DB round-trip; tests can use it
// to assert that any new event constant is also added to the map.
func (e SecurityEvent) IsKnown() bool {
	_, ok := allSecurityEvents[e]
	return ok
}

// IsKnown reports whether e is in the controlled vocabulary.
func (e DataEvent) IsKnown() bool {
	_, ok := allDataEvents[e]
	return ok
}

// SplitLogger is the port new code uses to write into the split
// ledgers. The old Logger interface continues to exist for callers
// still writing into the legacy `audit_log` table; new code MUST use
// SplitLogger.
//
// WriteSecurity persists a SecurityEvent into audit_log_security.
// `event.TenantID` MAY be nil (master-context events such as
// SecurityEventMasterGrant have no tenant). Implementations MUST
// reject events whose Event is not in the controlled vocabulary.
//
// WriteData persists a DataEvent into audit_log_data. `event.TenantID`
// MUST be non-nil — every PII-access event happens inside a tenant.
//
// Both methods MUST persist synchronously and return a non-nil error
// when the write did not commit. Non-repudiation requires that callers
// can refuse to proceed when the trail cannot be written.
type SplitLogger interface {
	WriteSecurity(ctx context.Context, event SecurityAuditEvent) error
	WriteData(ctx context.Context, event DataAuditEvent) error
}

// SecurityAuditEvent maps 1:1 onto audit_log_security columns.
//
// TenantID is optional: master-context events (master_grant,
// key_rotation) MAY omit it. ActorUserID is required for every event
// the split writer accepts; the actor for an unauthenticated
// login_fail is the user record being attempted (resolved upstream)
// or — when no user exists — a sentinel actor id documented per
// caller.
type SecurityAuditEvent struct {
	Event       SecurityEvent
	ActorUserID uuid.UUID
	TenantID    *uuid.UUID
	Target      map[string]any
	OccurredAt  time.Time
}

// DataAuditEvent maps 1:1 onto audit_log_data columns.
//
// TenantID is required (the underlying column is NOT NULL) — the LGPD
// purge job needs every PII-access row to be tenant-scoped so the
// retention sweep is safe.
type DataAuditEvent struct {
	Event       DataEvent
	ActorUserID uuid.UUID
	TenantID    uuid.UUID
	Target      map[string]any
	OccurredAt  time.Time
}
