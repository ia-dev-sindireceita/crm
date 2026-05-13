package postgres

// SIN-62252 / ADR 0004 §4: postgres-backed implementation of
// audit.SplitLogger. SIN-62424 (Phase B.2) retired the legacy
// AuditLogger and the `audit_log` table in migration 0015, leaving
// SplitAuditLogger as the only audit writer.
//
// The writer uses the dedicated `app_audit` pool created in migration
// 0009 and granted INSERT on the split tables in migration 0014. The
// pool is BYPASSRLS: tenant_id is supplied explicitly by the caller,
// not derived from session state, so the writer must not depend on
// app.tenant_id being set.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

// ErrSplitAuditEventInvalid signals that a SecurityAuditEvent or
// DataAuditEvent does not satisfy the boundary invariants enforced by
// SplitAuditLogger. Distinct from a wrapped pgx error so callers can
// short-circuit retries.
var ErrSplitAuditEventInvalid = errors.New("postgres: invalid split audit event")

// AuditExecutor is the minimal pool surface SplitAuditLogger needs.
// *pgxpool.Pool satisfies it. cmd/server passes the dedicated
// app_audit pool. SIN-62424 (Phase B.2) moved this declaration here
// from the deleted audit_logger.go.
type AuditExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// SplitAuditLogger is the postgres implementation of audit.SplitLogger.
//
// IMPORTANT: db MUST be the dedicated app_audit pool. Wiring app_runtime
// here would couple writes to the per-tenant RLS scope and re-introduce
// the non-repudiation hole the dedicated role exists to close.
type SplitAuditLogger struct {
	db AuditExecutor
}

// NewSplitAuditLogger wires SplitAuditLogger around a pool. ErrNilPool
// fires eagerly so cmd/server fails at boot rather than on the first
// audited event.
func NewSplitAuditLogger(db AuditExecutor) (*SplitAuditLogger, error) {
	if db == nil {
		return nil, ErrNilPool
	}
	return &SplitAuditLogger{db: db}, nil
}

const splitSecurityInsertSQL = `
INSERT INTO audit_log_security (id, tenant_id, actor_user_id, event_type, target, occurred_at)
VALUES (
  gen_random_uuid(),
  $1,
  $2,
  $3,
  $4::jsonb,
  COALESCE($5, now())
)
`

const splitDataInsertSQL = `
INSERT INTO audit_log_data (id, tenant_id, actor_user_id, event_type, target, occurred_at)
VALUES (
  gen_random_uuid(),
  $1,
  $2,
  $3,
  $4::jsonb,
  COALESCE($5, now())
)
`

// WriteSecurity persists a row into audit_log_security.
//
// Validation order (boundary first → DB last) mirrors the existing
// AuditLogger.Log: an event that fails validation never reaches the
// pool so the middleware can fail closed without a partial trail.
//
// TenantID is allowed to be nil (master-context events). When nil, the
// underlying column is NULL — see migration 0012 for the policy that
// permits this only for app_master_ops.
func (l *SplitAuditLogger) WriteSecurity(ctx context.Context, event audit.SecurityAuditEvent) error {
	if l == nil || l.db == nil {
		return ErrNilPool
	}
	if !event.Event.IsKnown() {
		return fmt.Errorf("%w: unknown security event %q", ErrSplitAuditEventInvalid, event.Event)
	}
	if event.ActorUserID == uuid.Nil {
		return fmt.Errorf("%w: zero actor user id", ErrSplitAuditEventInvalid)
	}

	encoded, err := encodeTarget(event.Target)
	if err != nil {
		return err
	}

	var tenantArg any
	if event.TenantID != nil && *event.TenantID != uuid.Nil {
		tenantArg = *event.TenantID
	}

	var occurredArg any
	if !event.OccurredAt.IsZero() {
		occurredArg = event.OccurredAt
	}

	if _, err := l.db.Exec(ctx, splitSecurityInsertSQL,
		tenantArg,
		event.ActorUserID,
		string(event.Event),
		string(encoded),
		occurredArg,
	); err != nil {
		return fmt.Errorf("postgres: insert audit_log_security: %w", err)
	}
	return nil
}

// WriteData persists a row into audit_log_data.
//
// TenantID is required: the column is NOT NULL and the LGPD purge job
// relies on every PII-access row being tenant-scoped so retention
// sweeps are safe.
func (l *SplitAuditLogger) WriteData(ctx context.Context, event audit.DataAuditEvent) error {
	if l == nil || l.db == nil {
		return ErrNilPool
	}
	if !event.Event.IsKnown() {
		return fmt.Errorf("%w: unknown data event %q", ErrSplitAuditEventInvalid, event.Event)
	}
	if event.ActorUserID == uuid.Nil {
		return fmt.Errorf("%w: zero actor user id", ErrSplitAuditEventInvalid)
	}
	if event.TenantID == uuid.Nil {
		return fmt.Errorf("%w: zero tenant id (audit_log_data is tenant-scoped)", ErrSplitAuditEventInvalid)
	}

	encoded, err := encodeTarget(event.Target)
	if err != nil {
		return err
	}

	var occurredArg any
	if !event.OccurredAt.IsZero() {
		occurredArg = event.OccurredAt
	}

	if _, err := l.db.Exec(ctx, splitDataInsertSQL,
		event.TenantID,
		event.ActorUserID,
		string(event.Event),
		string(encoded),
		occurredArg,
	); err != nil {
		return fmt.Errorf("postgres: insert audit_log_data: %w", err)
	}
	return nil
}

// encodeTarget marshals the target jsonb. A nil map becomes `{}` so the
// row always contains a valid JSON object (the column DEFAULT also
// covers it, but this keeps the wire bytes deterministic for tests).
func encodeTarget(target map[string]any) ([]byte, error) {
	if target == nil {
		target = map[string]any{}
	}
	encoded, err := json.Marshal(target)
	if err != nil {
		return nil, fmt.Errorf("postgres: marshal audit target: %w", err)
	}
	return encoded, nil
}
