// Package funnelapplications is the pgx-backed adapter for the funnel
// engine's [ApplicationsRepo] port. It writes idempotency rows to the
// funnel_rule_applications table introduced in migration 0103.
//
// The adapter routes every call through postgres.WithTenant so the RLS
// GUC app.tenant_id is set before the row is read or written —
// app_runtime is the only role the production wiring uses, and the
// table's policies make tenant scoping mandatory.
//
// UNIQUE conflict semantics: the engine relies on
// (rule_id, message_id) collisions to detect "already applied". The
// adapter inspects the pgconn.PgError SQLSTATE 23505 and returns
// [engine.ErrAlreadyApplied] so the use-case treats the race as a
// no-op success.
//
// SIN-62960 (Fase 4 funnel rule engine — NATS consumer, child of
// [SIN-62197](/SIN/issues/SIN-62197)).
package funnelapplications

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/funnel/engine"
)

// Compile-time assertion: Store satisfies the engine port. A drift in
// the port shape fails this file's build before any caller notices.
var _ engine.ApplicationsRepo = (*Store)(nil)

// Store is the pgx-backed adapter. Construct via [New]; the pool MUST
// be the app_runtime pool so the RLS policies on
// funnel_rule_applications apply.
type Store struct {
	pool postgres.TxBeginner
}

// New wraps pool and returns a ready Store. A nil pool yields
// postgres.ErrNilPool so cmd/server fails fast at boot.
func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, postgres.ErrNilPool
	}
	return &Store{pool: pool}, nil
}

// IsApplied returns true when funnel_rule_applications already carries
// a row for (ruleID, messageID) under the tenant. The query is
// index-only thanks to the UNIQUE constraint covering the pair.
func (s *Store) IsApplied(ctx context.Context, tenantID, ruleID, messageID uuid.UUID) (bool, error) {
	if tenantID == uuid.Nil {
		return false, fmt.Errorf("funnelapplications/postgres: nil tenantID")
	}
	if ruleID == uuid.Nil {
		return false, fmt.Errorf("funnelapplications/postgres: nil ruleID")
	}
	if messageID == uuid.Nil {
		return false, fmt.Errorf("funnelapplications/postgres: nil messageID")
	}

	var found bool
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var dummy int
		row := tx.QueryRow(ctx, selectAppliedExists, ruleID, messageID)
		switch err := row.Scan(&dummy); {
		case err == nil:
			found = true
			return nil
		case errors.Is(err, pgx.ErrNoRows):
			found = false
			return nil
		default:
			return err
		}
	})
	if err != nil {
		return false, fmt.Errorf("funnelapplications/postgres: IsApplied: %w", err)
	}
	return found, nil
}

// Record persists the application row. A UNIQUE conflict on
// (rule_id, message_id) is mapped to [engine.ErrAlreadyApplied] so the
// engine treats the race as success.
func (s *Store) Record(ctx context.Context, app engine.Application) error {
	if app.TenantID == uuid.Nil {
		return fmt.Errorf("funnelapplications/postgres: nil tenantID")
	}
	if app.RuleID == uuid.Nil {
		return fmt.Errorf("funnelapplications/postgres: nil ruleID")
	}
	if app.MessageID == uuid.Nil {
		return fmt.Errorf("funnelapplications/postgres: nil messageID")
	}
	if app.ConversationID == uuid.Nil {
		return fmt.Errorf("funnelapplications/postgres: nil conversationID")
	}
	if app.ActionType == "" {
		return fmt.Errorf("funnelapplications/postgres: blank actionType")
	}

	return postgres.WithTenant(ctx, s.pool, app.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, insertApplication,
			app.TenantID, app.RuleID, app.MessageID, app.ConversationID,
			app.ActionType, app.AppliedAt,
		)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return engine.ErrAlreadyApplied
			}
			return fmt.Errorf("funnelapplications/postgres: Record: %w", err)
		}
		return nil
	})
}

// selectAppliedExists is the dedup-presence probe. The 23505 UNIQUE
// constraint on (rule_id, message_id) means a row matches at most once.
const selectAppliedExists = `
	SELECT 1
	  FROM funnel_rule_applications
	 WHERE rule_id = $1 AND message_id = $2
	 LIMIT 1
`

// insertApplication is the idempotency-write path. Tenant id is set
// explicitly because the RLS policy's WITH CHECK clause demands it
// even though the GUC is also app.tenant_id (a defence-in-depth
// posture mirrored across the Fase 4 adapters).
const insertApplication = `
	INSERT INTO funnel_rule_applications
	  (tenant_id, rule_id, message_id, conversation_id, action_type, applied_at)
	VALUES ($1, $2, $3, $4, $5, $6)
`
