package postgres

// SIN-63184 tenant user_recovery_code adapter. Tenant counterpart of
// MasterRecoveryCodes from 0086 — same RecoveryStore port, distinct
// table with RLS isolation. AC #6 names the consumed column as
// used_at (rather than master's consumed_at); the SQL maps both
// names to mfa.RecoveryCodeRecord identically.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/iam/mfa"
)

var _ mfa.RecoveryStore = (*TenantUserRecoveryCodes)(nil)

// TenantUserRecoveryCodes is the tenant-scope adapter for the
// user_recovery_code table.
type TenantUserRecoveryCodes struct {
	pool     *pgxpool.Pool
	tenantID uuid.UUID
}

// NewTenantUserRecoveryCodes validates inputs and returns the adapter.
func NewTenantUserRecoveryCodes(pool *pgxpool.Pool, tenantID uuid.UUID) (*TenantUserRecoveryCodes, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	if tenantID == uuid.Nil {
		return nil, ErrZeroTenant
	}
	return &TenantUserRecoveryCodes{pool: pool, tenantID: tenantID}, nil
}

// InsertHashes adds N freshly-generated, Argon2id-hashed codes for the
// given user in a single statement.
func (a *TenantUserRecoveryCodes) InsertHashes(ctx context.Context, userID uuid.UUID, hashes []string) error {
	if userID == uuid.Nil {
		return fmt.Errorf("postgres: TenantUserRecoveryCodes.InsertHashes: userID is nil")
	}
	if len(hashes) == 0 {
		return fmt.Errorf("postgres: TenantUserRecoveryCodes.InsertHashes: empty hashes slice")
	}
	return WithTenant(ctx, a.pool, a.tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO user_recovery_code (user_id, tenant_id, code_hash)
			SELECT $1, $2, h FROM unnest($3::text[]) AS h
		`, userID, a.tenantID, hashes)
		if err != nil {
			return fmt.Errorf("postgres: TenantUserRecoveryCodes.InsertHashes exec: %w", err)
		}
		return nil
	})
}

// ListActive returns the not-yet-consumed rows for userID. Order is by
// id (PK uuid). The verifier walks every row exhaustively to avoid a
// timing oracle.
func (a *TenantUserRecoveryCodes) ListActive(ctx context.Context, userID uuid.UUID) ([]mfa.RecoveryCodeRecord, error) {
	var rows []mfa.RecoveryCodeRecord
	err := WithTenant(ctx, a.pool, a.tenantID, func(tx pgx.Tx) error {
		r, err := tx.Query(ctx,
			`SELECT id, code_hash
			   FROM user_recovery_code
			  WHERE user_id = $1 AND used_at IS NULL
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
		return nil, fmt.Errorf("postgres: TenantUserRecoveryCodes.ListActive: %w", err)
	}
	return rows, nil
}

// MarkConsumed sets used_at = now() on the named row. Idempotent via
// COALESCE so a retried verify path does not clobber the audit record
// of when the code was *first* used.
func (a *TenantUserRecoveryCodes) MarkConsumed(ctx context.Context, codeID uuid.UUID) error {
	if codeID == uuid.Nil {
		return fmt.Errorf("postgres: TenantUserRecoveryCodes.MarkConsumed: codeID is nil")
	}
	return WithTenant(ctx, a.pool, a.tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE user_recovery_code
			    SET used_at = COALESCE(used_at, now())
			  WHERE id = $1`,
			codeID,
		)
		if err != nil {
			return fmt.Errorf("postgres: TenantUserRecoveryCodes.MarkConsumed exec: %w", err)
		}
		return nil
	})
}

// InvalidateAll bulk-marks every active code for userID as used.
// Returns the number of rows touched so the caller can audit the
// regen size.
func (a *TenantUserRecoveryCodes) InvalidateAll(ctx context.Context, userID uuid.UUID) (int, error) {
	if userID == uuid.Nil {
		return 0, fmt.Errorf("postgres: TenantUserRecoveryCodes.InvalidateAll: userID is nil")
	}
	var affected int
	err := WithTenant(ctx, a.pool, a.tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE user_recovery_code
			    SET used_at = now()
			  WHERE user_id = $1 AND used_at IS NULL`,
			userID,
		)
		if err != nil {
			return fmt.Errorf("exec: %w", err)
		}
		affected = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("postgres: TenantUserRecoveryCodes.InvalidateAll: %w", err)
	}
	return affected, nil
}
