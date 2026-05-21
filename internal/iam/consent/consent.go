package consent

import (
	"context"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SubjectType enumerates the LGPD subject categories a consent row
// can attach to. The values mirror the CHECK constraint in migration
// 0107 (subject_type IN ('user','contact','tenant')); adding a value
// here without extending the migration produces a SQL CHECK violation
// at write time.
//
// "user" is a tenant operator (a row in users).
// "contact" is an end-customer (a row in contacts).
// "tenant" is the tenant itself accepting consent on behalf of its
// account-level relationship — used for ToS at signup.
type SubjectType string

const (
	SubjectUser    SubjectType = "user"
	SubjectContact SubjectType = "contact"
	SubjectTenant  SubjectType = "tenant"
)

var validSubjectTypes = map[SubjectType]struct{}{
	SubjectUser:    {},
	SubjectContact: {},
	SubjectTenant:  {},
}

// IsValid reports whether s is one of the allowed subject types.
func (s SubjectType) IsValid() bool {
	_, ok := validSubjectTypes[s]
	return ok
}

// Purpose enumerates the LGPD-purpose vocabulary the ledger accepts.
// Wire-stable: the literals are persisted in the purpose column and
// gated by the CHECK constraint on consent_record. Renaming or
// removing a literal is a breaking change that needs a migration plan.
type Purpose string

const (
	PurposeTermsOfService   Purpose = "terms_of_service"
	PurposePrivacyPolicy    Purpose = "privacy_policy"
	PurposeMarketing        Purpose = "marketing"
	PurposeCookiesAnalytics Purpose = "cookies_analytics"
)

var validPurposes = map[Purpose]struct{}{
	PurposeTermsOfService:   {},
	PurposePrivacyPolicy:    {},
	PurposeMarketing:        {},
	PurposeCookiesAnalytics: {},
}

// IsValid reports whether p is one of the allowed purposes.
func (p Purpose) IsValid() bool {
	_, ok := validPurposes[p]
	return ok
}

// Subject pairs a SubjectType with its identifier so callers cannot
// accidentally swap "user" + a contact id and produce a row that
// passes the per-column CHECK clauses but points at the wrong row.
// ID is a string because the column is TEXT — uuid-shaped subjects
// stringify at the boundary so a future non-uuid subject category
// (e.g. external_id) does not force a schema change.
type Subject struct {
	Type SubjectType
	ID   string
}

// IsValid reports whether s carries a valid SubjectType and a
// non-blank ID. The adapter applies the same checks via the CHECK
// constraint and NOT NULL column; the domain rejects earlier so
// callers see typed sentinels.
func (s Subject) IsValid() bool {
	if !s.Type.IsValid() {
		return false
	}
	return strings.TrimSpace(s.ID) != ""
}

// ConsentRecord mirrors one row of consent_record (migration 0107).
// The shape matches the migration column-for-column so the adapter
// is a thin Scan/Exec layer.
//
// ID is uuid.Nil for callers passing the record to Record — the
// adapter populates it from the INSERT RETURNING clause (or from a
// follow-up SELECT on idempotent no-op).
//
// IP uses netip.Addr to keep "no IP" as a typed zero value rather
// than a sentinel string; the adapter marshals to PostgreSQL's
// inet type via Addr.String(). For background jobs (no caller IP)
// pass the zero value and the adapter writes NULL.
//
// UserAgent is a free-form string; the adapter persists it verbatim
// up to a 4 KiB clamp. Callers are responsible for stripping PII
// from UA strings — the audit row is not anonymized.
type ConsentRecord struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	Subject      Subject
	Purpose      Purpose
	Version      string
	Granted      bool
	GrantedAt    time.Time
	RevokedAt    *time.Time
	RevokeReason string
	IP           netip.Addr
	UserAgent    string
}

// RevokeQuery names a single revocation request: locate the latest
// active grant for (TenantID, Subject, Purpose), flip granted=false,
// stamp RevokedAt=now() and RevokeReason=Reason. IP and UserAgent
// attach to the audit row emitted by RecordingRegistry.
type RevokeQuery struct {
	TenantID  uuid.UUID
	Subject   Subject
	Purpose   Purpose
	Reason    string
	IP        netip.Addr
	UserAgent string
}

// ConsentRegistry is the storage port for consent_record rows.
//
// Adapters MUST:
//
//   - On Record, INSERT one row keyed by
//     (tenant_id, subject_type, subject_id, purpose, version).
//     A repeat call with the same triple MUST be idempotent —
//     adapters resolve this via ON CONFLICT DO NOTHING and report
//     `created=false` so the decorator can skip the audit emission.
//   - On Latest, return the row with the most recent GrantedAt for
//     (Subject, Purpose) or (nil, nil) when none exist.
//   - On History, return every row for (Subject, Purpose) ordered by
//     GrantedAt DESC. The slice is empty (not nil) when no rows match.
//   - On Revoke, find the most recent active grant (Granted=true,
//     RevokedAt IS NULL) for (Subject, Purpose), flip the row, and
//     return the updated record. Adapters MUST return
//     ErrNoActiveGrant when no active grant exists. A second Revoke
//     on the same (Subject, Purpose) without a re-grant is therefore
//     ErrNoActiveGrant, not a silent no-op — callers can distinguish.
//
// Tenant scoping flows through ctx (the adapter sets `app.tenant_id`
// from the TenantID on the record/query). RLS gates every read and
// write so two tenants cannot see each other's rows even if they
// share a Subject.ID literal.
type ConsentRegistry interface {
	// Record persists rec. Returns the persisted row (with ID,
	// GrantedAt, etc. populated) and a created flag that is true
	// when a new row was inserted, false when the row already
	// existed and the call was a no-op.
	Record(ctx context.Context, rec ConsentRecord) (ConsentRecord, bool, error)

	// Latest returns the most-recent-by-GrantedAt row for
	// (tenant, subject, purpose) or (nil, nil) when no row matches.
	Latest(ctx context.Context, tenant uuid.UUID, subject Subject, purpose Purpose) (*ConsentRecord, error)

	// History returns every row for (tenant, subject, purpose),
	// ordered by GrantedAt DESC. An empty slice (not nil) signals
	// no rows.
	History(ctx context.Context, tenant uuid.UUID, subject Subject, purpose Purpose) ([]ConsentRecord, error)

	// Revoke flips the most recent active grant for
	// (TenantID, Subject, Purpose) into the revoked state and
	// returns the updated row. Returns ErrNoActiveGrant when no
	// active grant exists.
	Revoke(ctx context.Context, q RevokeQuery) (ConsentRecord, error)
}
