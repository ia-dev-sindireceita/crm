package postgres

// SIN-62342 master_mfa adapter. Wraps the runtime pool under
// WithMasterOps for an actorID supplied at construction so every read
// and write is logged by the master_ops_audit trigger from migration
// 0002 (ADR 0072 §master ops). Plaintext TOTP seeds MUST NOT travel
// through this adapter — the caller encrypts the seed with the
// SeedCipher (mfa port) before calling StoreSeed; LoadSeed returns
// the same opaque ciphertext for the caller to decrypt.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// Compile-time assertion: MasterMFA satisfies the domain port.
var _ mfa.SeedRepository = (*MasterMFA)(nil)

// MasterMFA is the master-scope adapter for the master_mfa table. The
// actorID captured at construction names the human operator currently
// driving the master console; it lands in the master_ops_audit trigger
// row for every change.
type MasterMFA struct {
	pool    *pgxpool.Pool
	actorID uuid.UUID
}

// NewMasterMFA validates inputs and returns an adapter ready for use.
// nil pool returns ErrNilPool so callers fail fast at construction
// rather than panic at first query. uuid.Nil actorID is rejected — a
// master operation without an audit actor would trip the trigger's
// "master_ops_actor_user_id GUC" guard at runtime, so reject it here
// for a louder error.
func NewMasterMFA(pool *pgxpool.Pool, actorID uuid.UUID) (*MasterMFA, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	if actorID == uuid.Nil {
		return nil, ErrZeroActor
	}
	return &MasterMFA{pool: pool, actorID: actorID}, nil
}

// StoreSeed upserts the master_mfa row for userID. Re-enrolling a
// master with a fresh seed is the regenerate-from-recovery path
// (ADR 0074 §5) — the row's enrolled_at is left untouched on update so
// the original audit trail is preserved, but reenroll_required is
// cleared and last_verified_at is reset to NULL (the user has not
// verified the *new* seed yet).
//
// seedCiphertext MUST be the output of mfa.SeedCipher.Encrypt — this
// adapter does not encrypt and writes whatever bytes it gets. Empty
// ciphertext is rejected to fail closed against a misconfigured
// caller.
func (a *MasterMFA) StoreSeed(ctx context.Context, userID uuid.UUID, seedCiphertext []byte) error {
	if userID == uuid.Nil {
		return fmt.Errorf("postgres: MasterMFA.StoreSeed: userID is nil")
	}
	if len(seedCiphertext) == 0 {
		return fmt.Errorf("postgres: MasterMFA.StoreSeed: empty ciphertext")
	}
	return WithMasterOps(ctx, a.pool, a.actorID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO master_mfa (user_id, totp_seed_encrypted)
			VALUES ($1, $2)
			ON CONFLICT (user_id) DO UPDATE
			  SET totp_seed_encrypted = EXCLUDED.totp_seed_encrypted,
			      reenroll_required   = false,
			      last_verified_at    = NULL
		`, userID, seedCiphertext)
		if err != nil {
			return fmt.Errorf("postgres: MasterMFA.StoreSeed exec: %w", err)
		}
		return nil
	})
}

// LoadSeed returns the encrypted seed for userID. Missing row is a
// distinct outcome from a database error — translated into the
// domain sentinel mfa.ErrNotEnrolled so the Service layer can
// errors.Is without importing pgx (Hexagonal rule from ADR 0074).
func (a *MasterMFA) LoadSeed(ctx context.Context, userID uuid.UUID) ([]byte, error) {
	var ciphertext []byte
	err := WithMasterOps(ctx, a.pool, a.actorID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT totp_seed_encrypted FROM master_mfa WHERE user_id = $1`,
			userID,
		).Scan(&ciphertext)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, mfa.ErrNotEnrolled
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: MasterMFA.LoadSeed: %w", err)
	}
	return ciphertext, nil
}

// MarkVerified stamps last_verified_at = now() on the master's row.
// Called by the verify handler (ADR 0074 §4) on every successful TOTP
// or recovery code submission so dashboards can age out unused master
// accounts. Returns nil when the user is not enrolled — the caller is
// expected to have already loaded the seed before reaching this path.
func (a *MasterMFA) MarkVerified(ctx context.Context, userID uuid.UUID) error {
	return WithMasterOps(ctx, a.pool, a.actorID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE master_mfa SET last_verified_at = now() WHERE user_id = $1`,
			userID,
		)
		if err != nil {
			return fmt.Errorf("postgres: MasterMFA.MarkVerified: %w", err)
		}
		return nil
	})
}

// MarkReenrollRequired sets reenroll_required = true on the master's
// row. Called when a recovery code is consumed (ADR 0074 §5) so the
// next session forces a fresh enrol. Returns nil if no row exists —
// the caller is the verify handler which has already loaded the seed.
func (a *MasterMFA) MarkReenrollRequired(ctx context.Context, userID uuid.UUID) error {
	return WithMasterOps(ctx, a.pool, a.actorID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE master_mfa SET reenroll_required = true WHERE user_id = $1`,
			userID,
		)
		if err != nil {
			return fmt.Errorf("postgres: MasterMFA.MarkReenrollRequired: %w", err)
		}
		return nil
	})
}
