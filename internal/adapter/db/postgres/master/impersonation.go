package master

// SIN-63958 / master-impersonation-spec §1.2: pgx-backed adapter for the
// master_impersonation_session table. Every method runs under
// postgres.WithMasterOps so the master_ops_audit_trigger from migration
// 0002 writes one master_ops_audit row per change. The pool MUST be the
// app_master_ops pool.
//
// Error mapping (boundary contract with internal/iam/impersonation):
//
//   "23505" UniqueViolation → ErrAlreadyActive
//   "23514" CheckViolation  → ErrInvalidReason
//   pgx.ErrNoRows           → ErrNoActiveImpersonation
//   any other error         → wrapped with %w (no sentinel)
//
// Codes are inlined as string literals to match the rest of the repo
// (see contacts/contacts.go, pix/eventlog.go); pulling in pgerrcode as
// a direct dep just for two constants doesn't pay for itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/iam/impersonation"
)

// Postgres SQLSTATE codes mapped to domain sentinels. Inlined to avoid
// adding github.com/jackc/pgerrcode as a direct dep just for two
// constants.
const (
	pgCodeUniqueViolation = "23505"
	pgCodeCheckViolation  = "23514"
)

// Compile-time assertion that ImpersonationStore satisfies the domain
// port. Drift in the port signature fails the build here before the
// next caller-side rewire.
var _ impersonation.Repo = (*ImpersonationStore)(nil)

// ImpersonationStore is the pgx-backed repository for
// master_impersonation_session. Construct with NewImpersonationStore;
// the pool MUST be the app_master_ops pool. actorID is the master user
// currently driving the console — it is threaded into every
// WithMasterOps call so the audit trigger writes an attributable row.
type ImpersonationStore struct {
	pool postgresadapter.TxBeginner
}

// NewImpersonationStore validates inputs and returns the store. A nil
// pool fires ErrNilPool eagerly so cmd/server fails at boot rather
// than on the first impersonation call.
func NewImpersonationStore(pool *pgxpool.Pool) (*ImpersonationStore, error) {
	if pool == nil {
		return nil, postgresadapter.ErrNilPool
	}
	return &ImpersonationStore{pool: pool}, nil
}

// Start INSERTs a new master_impersonation_session row. expires_at is
// computed server-side as started_at + DefaultEnvelopeTTL — the caller
// MUST NOT supply it. UniqueViolation → ErrAlreadyActive; CHECK
// violation on reason length → ErrInvalidReason.
func (s *ImpersonationStore) Start(ctx context.Context, in impersonation.StartInput) (*impersonation.Session, error) {
	if in.MasterUserID == uuid.Nil {
		return nil, fmt.Errorf("master/postgres: impersonation start: %w", postgresadapter.ErrZeroActor)
	}
	if in.MasterSessionID == uuid.Nil {
		return nil, errors.New("master/postgres: impersonation start: master_session_id is uuid.Nil")
	}
	if in.TargetTenantID == uuid.Nil {
		return nil, errors.New("master/postgres: impersonation start: target_tenant_id is uuid.Nil")
	}
	startedAt := in.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	expiresAt := startedAt.Add(impersonation.DefaultEnvelopeTTL)
	id := uuid.New()

	err := postgresadapter.WithMasterOps(ctx, s.pool, in.MasterUserID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO master_impersonation_session
			  (id, master_user_id, master_session_id, target_tenant_id,
			   reason, started_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			id, in.MasterUserID, in.MasterSessionID, in.TargetTenantID,
			in.Reason, startedAt, expiresAt,
		)
		return mapPGError(err)
	})
	if err != nil {
		return nil, err
	}
	return &impersonation.Session{
		ID:              id,
		MasterUserID:    in.MasterUserID,
		MasterSessionID: in.MasterSessionID,
		TargetTenantID:  in.TargetTenantID,
		Reason:          in.Reason,
		StartedAt:       startedAt,
		ExpiresAt:       expiresAt,
	}, nil
}

// ActiveForSession returns the single active (ended_at IS NULL) row
// for masterSessionID. The partial unique index guarantees at most one
// match. ErrNoActiveImpersonation when none.
func (s *ImpersonationStore) ActiveForSession(ctx context.Context, masterSessionID uuid.UUID) (*impersonation.Session, error) {
	if masterSessionID == uuid.Nil {
		return nil, impersonation.ErrNoActiveImpersonation
	}
	var out impersonation.Session
	err := postgresadapter.WithMasterOps(ctx, s.pool, masterSessionID, func(tx pgx.Tx) error {
		// We use the masterSessionID as the WithMasterOps actorID for
		// the read-only path: the actorID GUC is informational on the
		// session_open row only, and a read carries no audit
		// attribution beyond it. The Start handler will rewrite under
		// the real user id when it writes.
		return scanSession(tx.QueryRow(ctx, `
			SELECT id, master_user_id, master_session_id, target_tenant_id,
			       reason, started_at, expires_at, ended_at, ended_reason
			  FROM master_impersonation_session
			 WHERE master_session_id = $1
			   AND ended_at IS NULL`, masterSessionID), &out)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, impersonation.ErrNoActiveImpersonation
		}
		return nil, err
	}
	return &out, nil
}

// End UPDATEs ended_at + ended_reason on the row. The WHERE clause
// guards on ended_at IS NULL so a second concurrent End collapses to
// rowsAffected==0 → ErrNoActiveImpersonation. actor is the master user
// driving the End and is threaded into WithMasterOps so the
// master_ops_audit trigger writes a row attributable to the human —
// passing the impersonation row id here would silently corrupt the
// audit trail (the trigger reads app.master_ops_actor_user_id from the
// GUC).
func (s *ImpersonationStore) End(ctx context.Context, id uuid.UUID, actor uuid.UUID, reason string, at time.Time) error {
	if id == uuid.Nil {
		return impersonation.ErrNoActiveImpersonation
	}
	if actor == uuid.Nil {
		return fmt.Errorf("master/postgres: end impersonation: %w", postgresadapter.ErrZeroActor)
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return postgresadapter.WithMasterOps(ctx, s.pool, actor, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE master_impersonation_session
			   SET ended_at = $2,
			       ended_reason = $3
			 WHERE id = $1
			   AND ended_at IS NULL`,
			id, at, reason,
		)
		if err != nil {
			return fmt.Errorf("master/postgres: end impersonation: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return impersonation.ErrNoActiveImpersonation
		}
		return nil
	})
}

// ListAuditByCorrelation returns up to limit audit_log_security rows
// tagged with correlationID, ordered by occurred_at ASC. The Feed SSE
// handler polls this once per second; the cap protects the process
// from a long-running envelope.
func (s *ImpersonationStore) ListAuditByCorrelation(ctx context.Context, correlationID uuid.UUID, limit int) ([]audit.SecurityRow, error) {
	if correlationID == uuid.Nil {
		return nil, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var out []audit.SecurityRow
	err := postgresadapter.WithMasterOps(ctx, s.pool, correlationID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, actor_user_id, event_type,
			       correlation_id, target, occurred_at
			  FROM audit_log_security
			 WHERE correlation_id = $1
			 ORDER BY occurred_at ASC
			 LIMIT $2`, correlationID, limit)
		if err != nil {
			return fmt.Errorf("master/postgres: query audit feed: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				row           audit.SecurityRow
				tenantID      *uuid.UUID
				correlationID *uuid.UUID
				eventStr      string
				targetRaw     []byte
			)
			if err := rows.Scan(&row.ID, &tenantID, &row.ActorUserID, &eventStr, &correlationID, &targetRaw, &row.OccurredAt); err != nil {
				return fmt.Errorf("master/postgres: scan audit row: %w", err)
			}
			row.TenantID = tenantID
			row.CorrelationID = correlationID
			row.Event = audit.SecurityEvent(eventStr)
			if len(targetRaw) > 0 {
				m := map[string]any{}
				if err := json.Unmarshal(targetRaw, &m); err == nil {
					row.Target = m
				}
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// scanSession reads a single master_impersonation_session row into
// dst. The shared helper exists so ActiveForSession and any future
// id-scoped read share the column order.
func scanSession(row pgx.Row, dst *impersonation.Session) error {
	var (
		endedAt     *time.Time
		endedReason *string
	)
	if err := row.Scan(
		&dst.ID,
		&dst.MasterUserID,
		&dst.MasterSessionID,
		&dst.TargetTenantID,
		&dst.Reason,
		&dst.StartedAt,
		&dst.ExpiresAt,
		&endedAt,
		&endedReason,
	); err != nil {
		return err
	}
	dst.EndedAt = endedAt
	if endedReason != nil {
		dst.EndedReason = *endedReason
	}
	return nil
}

// mapPGError translates pgconn.PgError codes into the domain sentinels.
// Anything we don't recognise wraps the original error so call sites
// can still inspect the wire-level cause.
func mapPGError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgCodeUniqueViolation:
			return impersonation.ErrAlreadyActive
		case pgCodeCheckViolation:
			return impersonation.ErrInvalidReason
		}
	}
	return fmt.Errorf("master/postgres: impersonation: %w", err)
}
