package consent

import "errors"

// ErrInvalidTenant signals that ConsentRecord.TenantID or
// RevokeQuery.TenantID is uuid.Nil. The column is NOT NULL and every
// row is tenant-scoped via RLS; the domain rejects the boundary
// before the adapter so callers see a typed sentinel.
var ErrInvalidTenant = errors.New("consent: invalid tenant id")

// ErrInvalidSubjectType signals that Subject.Type is outside the
// {user, contact, tenant} vocabulary. The CHECK constraint on
// consent_record enforces the same rule; the domain rejects earlier
// so a misconfigured caller sees a typed error instead of a SQL
// state code.
var ErrInvalidSubjectType = errors.New("consent: invalid subject type")

// ErrInvalidSubjectID signals that Subject.ID is blank after
// trimming. subject_id is NOT NULL; the domain pre-validates so the
// adapter does not have to translate a generic NOT NULL violation.
var ErrInvalidSubjectID = errors.New("consent: invalid subject id")

// ErrInvalidPurpose signals that Purpose is outside the four
// non-AI LGPD purposes accepted by consent_record. Same rationale
// as ErrInvalidSubjectType.
var ErrInvalidPurpose = errors.New("consent: invalid purpose")

// ErrInvalidVersion signals that ConsentRecord.Version is blank
// after trimming. version is NOT NULL because "no version" would
// match every Latest/History query under the same (subject,
// purpose) probe and silently collapse history.
var ErrInvalidVersion = errors.New("consent: invalid version")

// ErrInvalidRevokeReason signals that RevokeQuery.Reason is blank
// after trimming. Reason is required for non-repudiation: an LGPD
// auditor reading audit_log_data must always be able to answer
// "why was this consent revoked" without trusting the operator's
// memory.
var ErrInvalidRevokeReason = errors.New("consent: invalid revoke reason")

// ErrNoActiveGrant is returned by Revoke when no active grant
// exists for (TenantID, Subject, Purpose). The caller can
// distinguish this from a transport error and present an idempotent
// "already revoked" UI rather than retrying.
var ErrNoActiveGrant = errors.New("consent: no active grant to revoke")

// ErrNilStore is returned by NewRecordingRegistry when the inner
// ConsentRegistry is nil. The decorator guards construction-time so
// cmd/server fails to start rather than panicking on the first call.
var ErrNilStore = errors.New("consent: ConsentRegistry is required")

// ErrNilAuditLogger is returned by NewRecordingRegistry when the
// supplied SplitLogger is nil. Same fail-fast rationale as
// ErrNilStore: an audit-less decorator would silently drop the LGPD
// trail.
var ErrNilAuditLogger = errors.New("consent: audit logger is required")
