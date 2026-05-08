package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

// ErrAuditEventInvalid signals that the AuditEvent does not satisfy the
// minimum non-repudiation invariants enforced by AuditLogger. Distinct
// from a wrapped pgx error so callers can short-circuit retries.
var ErrAuditEventInvalid = errors.New("postgres: invalid audit event")

// AuditExecutor is the minimal pool surface AuditLogger needs.
// *pgxpool.Pool satisfies it. cmd/server passes the dedicated
// app_audit pool (BYPASSRLS, INSERT-only on audit_log per
// migration 0009).
type AuditExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// AuditLogger is the postgres-backed implementation of audit.Logger.
//
// IMPORTANT: the pool MUST be the dedicated app_audit pool (BYPASSRLS,
// INSERT-only on audit_log). Wiring app_runtime here would couple
// audit writes to the per-tenant RLS scope and re-introduce the
// non-repudiation hole the dedicated role exists to close.
type AuditLogger struct {
	db AuditExecutor
}

// NewAuditLogger wires AuditLogger around a pool. ErrNilPool fires
// eagerly so cmd/server fails at boot rather than on the first
// impersonation request.
func NewAuditLogger(db AuditExecutor) (*AuditLogger, error) {
	if db == nil {
		return nil, ErrNilPool
	}
	return &AuditLogger{db: db}, nil
}

// auditInsertSQL writes one row. created_at is sent as $5 so tests can
// pin timestamps; passing the zero time tells the column DEFAULT to
// fire (the server-side now()).
const auditInsertSQL = `
INSERT INTO audit_log (id, tenant_id, actor_user_id, event, target, created_at)
VALUES (
  gen_random_uuid(),
  $1,
  $2,
  $3,
  $4::jsonb,
  COALESCE($5, now())
)
`

// Log persists a single AuditEvent. The tenant_id column is NOT NULL
// in migration 0007, so a nil event.TenantID returns
// ErrAuditEventInvalid before any DB round-trip happens — this lets
// the middleware fail closed (return 500 to the user) and leave no
// half-written trail.
func (l *AuditLogger) Log(ctx context.Context, event audit.AuditEvent) error {
	if l == nil || l.db == nil {
		return ErrNilPool
	}
	if event.Event == "" {
		return fmt.Errorf("%w: empty event name", ErrAuditEventInvalid)
	}
	if event.ActorUserID == uuid.Nil {
		return fmt.Errorf("%w: zero actor user id", ErrAuditEventInvalid)
	}
	if event.TenantID == nil || *event.TenantID == uuid.Nil {
		return fmt.Errorf("%w: nil/zero tenant id", ErrAuditEventInvalid)
	}

	target := event.Target
	if target == nil {
		target = map[string]any{}
	}
	encoded, err := json.Marshal(target)
	if err != nil {
		return fmt.Errorf("postgres: marshal audit target: %w", err)
	}

	var createdAt any
	if !event.CreatedAt.IsZero() {
		createdAt = event.CreatedAt
	}

	if _, err := l.db.Exec(ctx, auditInsertSQL,
		*event.TenantID,
		event.ActorUserID,
		event.Event,
		string(encoded),
		createdAt,
	); err != nil {
		return fmt.Errorf("postgres: insert audit_log: %w", err)
	}
	return nil
}
