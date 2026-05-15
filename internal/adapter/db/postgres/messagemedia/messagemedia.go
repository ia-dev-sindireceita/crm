// Package messagemedia is the pgx-backed adapter for the
// scanner.MessageMediaStore port (SIN-62804 / F2-05c). It patches the
// `message.media` jsonb column produced by migration 0092 with the
// verdict from MediaScanner.Scan, and enforces the AC's idempotency
// rule (no-op when scan_status is already clean/infected) at the SQL
// layer with a single conditional UPDATE.
//
// Tenant routing: every call runs through postgres.WithTenant so the
// RLS policies on `message` apply. Cross-tenant or RLS-hidden rows
// collapse to scanner.ErrNotFound (the worker treats those as poison
// messages and acks them so the dead-letter does not loop).
package messagemedia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/media/scanner"
)

// Store implements scanner.MessageMediaStore on top of an app_runtime
// pgxpool. Construct via New(pool); a nil pool yields ErrNilPool.
type Store struct {
	pool postgres.TxBeginner
}

// Compile-time guarantee the adapter still satisfies the domain port.
var _ scanner.MessageMediaStore = (*Store)(nil)

// New wraps pool. The pool MUST be the app_runtime pool so the RLS
// policies on `message` apply and a cross-tenant call cannot leak.
func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, postgres.ErrNilPool
	}
	return &Store{pool: pool}, nil
}

// UpdateScanResult patches `message.media -> scan_status` and
// `scan_engine` for the row identified by (tenantID, messageID).
//
// The single UPDATE statement is conditional on
// `media->>'scan_status' = 'pending'` so two redeliveries cannot race
// each other into different verdicts: the first transaction wins, the
// second touches zero rows and is reported back as ErrAlreadyFinalised
// without any further query (the existence check fans out only when
// nothing was updated, so the hot path is one round-trip).
func (s *Store) UpdateScanResult(
	ctx context.Context,
	tenantID, messageID uuid.UUID,
	result scanner.ScanResult,
) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("messagemedia: UpdateScanResult: tenant id is nil")
	}
	if messageID == uuid.Nil {
		return scanner.ErrNotFound
	}
	if !result.Status.Valid() || result.Status == scanner.StatusPending {
		return fmt.Errorf("messagemedia: UpdateScanResult: %q is not a terminal verdict", result.Status)
	}

	verdict, err := json.Marshal(map[string]string{
		"scan_status": string(result.Status),
		"scan_engine": result.EngineID,
	})
	if err != nil {
		return fmt.Errorf("messagemedia: marshal verdict: %w", err)
	}

	var affected int64
	dbErr := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `
			UPDATE message
			   SET media = COALESCE(media, '{}'::jsonb) || $1::jsonb
			 WHERE id = $2
			   AND COALESCE(media->>'scan_status', 'pending') = 'pending'
		`, verdict, messageID)
		if err != nil {
			return err
		}
		affected = ct.RowsAffected()
		if affected == 0 {
			// Distinguish "row not present (or RLS-hidden)" from
			// "row present but already terminal". One extra round-trip
			// only on the slow path; the worker acks both outcomes.
			var exists bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM message WHERE id = $1)
			`, messageID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return scanner.ErrNotFound
			}
			return scanner.ErrAlreadyFinalised
		}
		return nil
	})
	if errors.Is(dbErr, scanner.ErrNotFound) || errors.Is(dbErr, scanner.ErrAlreadyFinalised) {
		return dbErr
	}
	if dbErr != nil {
		return fmt.Errorf("messagemedia: UpdateScanResult: %w", dbErr)
	}
	return nil
}
