package postgres

// SIN-63184 tenant user_mfa adapter. The tenant counterpart of
// MasterMFA from 0086 — same SeedRepository port, distinct table with
// RLS isolation. tenantID is captured at construction so cmd/server
// can wire a per-tenant adapter at the HTTP boundary (mirroring
// TenantLockouts).
//
// Plaintext TOTP seeds MUST NOT travel through this adapter — the
// caller encrypts the seed with the SeedCipher (mfa port) before
// calling StoreSeed; LoadSeed returns the same opaque ciphertext for
// the caller to decrypt.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/iam/mfa"
)

var _ mfa.SeedRepository = (*TenantUserMFA)(nil)

// TenantUserMFA is the tenant-scope adapter for the user_mfa table.
type TenantUserMFA struct {
	pool     *pgxpool.Pool
	tenantID uuid.UUID
}

// NewTenantUserMFA validates inputs and returns an adapter ready for use.
func NewTenantUserMFA(pool *pgxpool.Pool, tenantID uuid.UUID) (*TenantUserMFA, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	if tenantID == uuid.Nil {
		return nil, ErrZeroTenant
	}
	return &TenantUserMFA{pool: pool, tenantID: tenantID}, nil
}

// StoreSeed upserts the user_mfa row for userID. Re-enrolling clears
// reenroll_required and resets last_verified_at — the user has not
// verified the *new* seed yet.
func (a *TenantUserMFA) StoreSeed(ctx context.Context, userID uuid.UUID, seedCiphertext []byte) error {
	if userID == uuid.Nil {
		return fmt.Errorf("postgres: TenantUserMFA.StoreSeed: userID is nil")
	}
	if len(seedCiphertext) == 0 {
		return fmt.Errorf("postgres: TenantUserMFA.StoreSeed: empty ciphertext")
	}
	return WithTenant(ctx, a.pool, a.tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO user_mfa (user_id, tenant_id, totp_seed_encrypted)
			VALUES ($1, $2, $3)
			ON CONFLICT (user_id) DO UPDATE
			  SET totp_seed_encrypted = EXCLUDED.totp_seed_encrypted,
			      reenroll_required   = false,
			      last_verified_at    = NULL
		`, userID, a.tenantID, seedCiphertext)
		if err != nil {
			return fmt.Errorf("postgres: TenantUserMFA.StoreSeed exec: %w", err)
		}
		return nil
	})
}

// LoadSeed returns the encrypted seed for userID. Missing row maps to
// mfa.ErrNotEnrolled so the Service layer can errors.Is without
// importing pgx.
func (a *TenantUserMFA) LoadSeed(ctx context.Context, userID uuid.UUID) ([]byte, error) {
	var ciphertext []byte
	err := WithTenant(ctx, a.pool, a.tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT totp_seed_encrypted FROM user_mfa WHERE user_id = $1`,
			userID,
		).Scan(&ciphertext)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, mfa.ErrNotEnrolled
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: TenantUserMFA.LoadSeed: %w", err)
	}
	return ciphertext, nil
}

// MarkVerified stamps last_verified_at = now() on the user's row.
func (a *TenantUserMFA) MarkVerified(ctx context.Context, userID uuid.UUID) error {
	return WithTenant(ctx, a.pool, a.tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE user_mfa SET last_verified_at = now() WHERE user_id = $1`,
			userID,
		)
		if err != nil {
			return fmt.Errorf("postgres: TenantUserMFA.MarkVerified: %w", err)
		}
		return nil
	})
}

// MarkReenrollRequired flags the row so the next session forces a
// fresh enrol. Called by ConsumeRecovery when a recovery code is used.
func (a *TenantUserMFA) MarkReenrollRequired(ctx context.Context, userID uuid.UUID) error {
	return WithTenant(ctx, a.pool, a.tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE user_mfa SET reenroll_required = true WHERE user_id = $1`,
			userID,
		)
		if err != nil {
			return fmt.Errorf("postgres: TenantUserMFA.MarkReenrollRequired: %w", err)
		}
		return nil
	})
}

// IsEnrolled reports whether the user has completed enrolment AND has
// not been flagged for re-enrol. The login gate uses this to decide
// whether to redirect to /admin/2fa/setup or /admin/2fa/verify.
func (a *TenantUserMFA) IsEnrolled(ctx context.Context, userID uuid.UUID) (bool, error) {
	var reenroll bool
	err := WithTenant(ctx, a.pool, a.tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT reenroll_required FROM user_mfa WHERE user_id = $1`,
			userID,
		).Scan(&reenroll)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("postgres: TenantUserMFA.IsEnrolled: %w", err)
	}
	return !reenroll, nil
}
