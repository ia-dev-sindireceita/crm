// Package impersonation is the domain layer for the session-bound,
// 15-minute, server-authoritative impersonation envelope (SIN-63958 /
// master-impersonation-spec §1). It defines:
//
//   - The Session aggregate (one row in master_impersonation_session).
//   - The Repo port that wraps the storage adapter behind a narrow
//     interface so the middleware + handlers can be unit-tested with
//     in-memory fakes.
//   - The sentinel errors callers compare against with errors.Is.
//
// This package MUST stay free of database, HTTP, or other infrastructure
// imports. The postgres adapter lives in
// internal/adapter/db/postgres/master/impersonation.go.
package impersonation

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

// DefaultEnvelopeTTL is the spec §1.2 hard ceiling. expires_at is
// computed server-side at INSERT time as started_at + DefaultEnvelopeTTL;
// the value is NEVER accepted from the client. A different deployment
// SHOULD compile-time override this in a follow-up rather than expose it
// as runtime config — the duration is a security guarantee, not a knob.
const DefaultEnvelopeTTL = 15 * time.Minute

// MinReasonLen / MaxReasonLen mirror the CHECK constraint in migration
// 0116 (length(reason) BETWEEN 8 AND 500). The handler validates at the
// boundary; the SQL check is the defense-in-depth backstop.
const (
	MinReasonLen = 8
	MaxReasonLen = 500
)

// Session is the in-memory shape of a master_impersonation_session row.
// EndedAt / EndedReason are zero-value when the envelope is still active.
type Session struct {
	ID              uuid.UUID
	MasterUserID    uuid.UUID
	MasterSessionID uuid.UUID
	TargetTenantID  uuid.UUID
	Reason          string
	StartedAt       time.Time
	ExpiresAt       time.Time
	EndedAt         *time.Time
	EndedReason     string
}

// IsActive reports whether the envelope has not been explicitly ended.
// Callers wanting "active AND not expired" should also compare
// clock() < ExpiresAt — IsActive deliberately does not consult a clock.
func (s Session) IsActive() bool { return s.EndedAt == nil }

// StartInput is the validated input to Repo.Start. Callers (the Start
// handler) populate every field; the adapter computes ExpiresAt server-
// side as StartedAt + DefaultEnvelopeTTL.
type StartInput struct {
	MasterUserID    uuid.UUID
	MasterSessionID uuid.UUID
	TargetTenantID  uuid.UUID
	Reason          string
	StartedAt       time.Time
}

// Repo is the storage port for the impersonation envelope. The postgres
// adapter is the production implementation; tests use an in-memory fake.
//
// Start INSERTs a new row with server-computed expires_at. UniqueViolation
// on the partial index → ErrAlreadyActive. CHECK violation on the reason
// length → ErrInvalidReason. Any other infra error wraps a non-sentinel.
//
// ActiveForSession returns the single active (ended_at IS NULL) row for
// the given master_session_id, or ErrNoActiveImpersonation when none.
//
// End UPDATEs ended_at + ended_reason on the row. Idempotent at the
// callsite — the middleware's expiry branch and the explicit /end
// handler both go through End; a second call returns ErrNoActiveImpersonation
// because the WHERE clause filters on ended_at IS NULL.
//
// actor is the master user whose action drove the End — it MUST be the
// master_user_id, NOT the session id. The postgres adapter threads it
// into postgres.WithMasterOps so master_ops_audit.actor_user_id records
// the human user, not the impersonation row. All call sites have the
// active session in hand (its MasterUserID field) before calling End.
//
// ListAuditByCorrelation returns the audit_log_security rows tagged with
// the given correlation_id, ordered by occurred_at ASC, capped at limit.
// The Feed SSE handler polls this once per second; the adapter MUST
// bound its result set so a long-running envelope cannot OOM the
// process.
type Repo interface {
	Start(ctx context.Context, in StartInput) (*Session, error)
	ActiveForSession(ctx context.Context, masterSessionID uuid.UUID) (*Session, error)
	End(ctx context.Context, id uuid.UUID, actor uuid.UUID, reason string, at time.Time) error
	ListAuditByCorrelation(ctx context.Context, id uuid.UUID, limit int) ([]audit.SecurityRow, error)
}

// ErrNoActiveImpersonation is the sentinel returned by ActiveForSession
// when no active envelope exists, and by End when the row is already
// ended (the UPDATE matched zero rows).
var ErrNoActiveImpersonation = errors.New("impersonation: no active envelope")

// ErrAlreadyActive is the sentinel returned by Start when the partial
// unique index trips — i.e. the same master_session already has an
// active envelope. The Start handler MUST translate this into a 409
// (spec §1.4) so the operator sees a meaningful retry path.
var ErrAlreadyActive = errors.New("impersonation: envelope already active for this master session")

// ErrInvalidReason is the sentinel returned by Start when the SQL
// CHECK constraint trips. The handler validates length(8..500) at the
// boundary; this sentinel exists so a defense-in-depth violation
// surfaces a 422 instead of a 500.
var ErrInvalidReason = errors.New("impersonation: reason must be 8..500 characters")
