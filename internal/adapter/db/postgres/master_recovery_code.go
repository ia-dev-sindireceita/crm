package postgres

// SIN-62342 master_recovery_code adapter. Stores the Argon2id hashes of
// the 10 single-use recovery codes minted at every enrol or
// regenerate (ADR 0074 §2). Plaintext codes never travel through this
// adapter; the caller has already hashed them via mfa.CodeHasher.
//
// All operations run under WithMasterOps so the master_ops_audit
// trigger from migration 0002 records every insert/update/delete.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// Compile-time assertion: MasterRecoveryCodes satisfies the domain port.
var _ mfa.RecoveryStore = (*MasterRecoveryCodes)(nil)

// MasterRecoveryCodes is the master-scope adapter for the
// master_recovery_code table. As with MasterMFA, the actorID is
// captured at construction so every audit row names the human
// operator.
type MasterRecoveryCodes struct {
	pool    *pgxpool.Pool
	actorID uuid.UUID
}

// NewMasterRecoveryCodes validates and returns the adapter.
func NewMasterRecoveryCodes(pool *pgxpool.Pool, actorID uuid.UUID) (*MasterRecoveryCodes, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	if actorID == uuid.Nil {
		return nil, ErrZeroActor
	}
	return &MasterRecoveryCodes{pool: pool, actorID: actorID}, nil
}

// InsertHashes adds N freshly-generated, Argon2id-hashed codes for the
// given master in a single statement. Postgres synthesises id and
// generated_at; consumed_at remains NULL until the code is used. An
// empty hashes slice is a programmer error and returns immediately
// without touching the DB so a regenerate flow that mints zero codes
// (a bug) does not silently succeed.
func (a *MasterRecoveryCodes) InsertHashes(ctx context.Context, userID uuid.UUID, hashes []string) error {
	if userID == uuid.Nil {
		return fmt.Errorf("postgres: MasterRecoveryCodes.InsertHashes: userID is nil")
	}
	if len(hashes) == 0 {
		return fmt.Errorf("postgres: MasterRecoveryCodes.InsertHashes: empty hashes slice")
	}
	return WithMasterOps(ctx, a.pool, a.actorID, func(tx pgx.Tx) error {
		// pgx's CopyFrom is overkill for ten rows — a single INSERT with
		// UNNEST keeps the path small and uses one round-trip.
		_, err := tx.Exec(ctx, `
			INSERT INTO master_recovery_code (user_id, code_hash)
			SELECT $1, h FROM unnest($2::text[]) AS h
		`, userID, hashes)
		if err != nil {
			return fmt.Errorf("postgres: MasterRecoveryCodes.InsertHashes exec: %w", err)
		}
		return nil
	})
}

// ListActive returns the not-yet-consumed code rows for userID. The
// verifier walks the slice in caller order, calling
// mfa.CodeHasher.Verify on each row's hash against the submitted
// plaintext. Order is by id (PK uuid) — stable but not
// cryptographically meaningful; the verifier MUST visit every row
// before declaring failure so a forged plaintext can not skip ahead.
func (a *MasterRecoveryCodes) ListActive(ctx context.Context, userID uuid.UUID) ([]mfa.RecoveryCodeRecord, error) {
	var rows []mfa.RecoveryCodeRecord
	err := WithMasterOps(ctx, a.pool, a.actorID, func(tx pgx.Tx) error {
		r, err := tx.Query(ctx,
			`SELECT id, code_hash
			   FROM master_recovery_code
			  WHERE user_id = $1 AND consumed_at IS NULL
			  ORDER BY id`,
			userID,
		)
		if err != nil {
			return fmt.Errorf("query: %w", err)
		}
		defer r.Close()
		for r.Next() {
			var rec mfa.RecoveryCodeRecord
			if err := r.Scan(&rec.ID, &rec.Hash); err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			rows = append(rows, rec)
		}
		return r.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: MasterRecoveryCodes.ListActive: %w", err)
	}
	return rows, nil
}

// MarkConsumed sets consumed_at = now() on the named row. Idempotent:
// COALESCE(consumed_at, now()) preserves the original timestamp on a
// double-call, so a retried verify path does not clobber the audit
// record of when the code was *first* used.
func (a *MasterRecoveryCodes) MarkConsumed(ctx context.Context, codeID uuid.UUID) error {
	if codeID == uuid.Nil {
		return fmt.Errorf("postgres: MasterRecoveryCodes.MarkConsumed: codeID is nil")
	}
	return WithMasterOps(ctx, a.pool, a.actorID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE master_recovery_code
			    SET consumed_at = COALESCE(consumed_at, now())
			  WHERE id = $1`,
			codeID,
		)
		if err != nil {
			return fmt.Errorf("postgres: MasterRecoveryCodes.MarkConsumed exec: %w", err)
		}
		return nil
	})
}

// InvalidateAll bulk-marks every active code for userID as consumed.
// Returns the number of rows touched so the caller can audit the
// regen size (ADR 0074 §2 expects 10; a different number is a bug
// worth surfacing).
func (a *MasterRecoveryCodes) InvalidateAll(ctx context.Context, userID uuid.UUID) (int, error) {
	if userID == uuid.Nil {
		return 0, fmt.Errorf("postgres: MasterRecoveryCodes.InvalidateAll: userID is nil")
	}
	var affected int
	err := WithMasterOps(ctx, a.pool, a.actorID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE master_recovery_code
			    SET consumed_at = now()
			  WHERE user_id = $1 AND consumed_at IS NULL`,
			userID,
		)
		if err != nil {
			return fmt.Errorf("exec: %w", err)
		}
		affected = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("postgres: MasterRecoveryCodes.InvalidateAll: %w", err)
	}
	return affected, nil
}
