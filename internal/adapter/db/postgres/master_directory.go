package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MasterDirectory satisfies mastermfa.MasterUserDirectory. It reads
// the email column from the users table for a master user row
// (is_master = true / tenant_id IS NULL) using the app_master_ops
// role via WithMasterOps.
type MasterDirectory struct {
	pool    TxBeginner
	actorID uuid.UUID
}

// NewMasterDirectory returns the adapter. Nil pool or zero actorID
// return errors consistent with the rest of the postgres package.
func NewMasterDirectory(pool *pgxpool.Pool, actorID uuid.UUID) (*MasterDirectory, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	if actorID == uuid.Nil {
		return nil, ErrZeroActor
	}
	return &MasterDirectory{pool: pool, actorID: actorID}, nil
}

// EmailFor returns the email of the master user with the given id.
// Returns ("", nil) when the row exists but email is empty (the
// interface contract from mastermfa.MasterUserDirectory). Returns
// ErrNotFound wrapped in a descriptive error when no row exists.
func (d *MasterDirectory) EmailFor(ctx context.Context, userID uuid.UUID) (string, error) {
	var email string
	err := WithMasterOps(ctx, d.pool, d.actorID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT email FROM users WHERE id = $1 AND is_master = true`,
			userID,
		).Scan(&email)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("postgres: MasterDirectory: user %s not found", userID)
	}
	if err != nil {
		return "", fmt.Errorf("postgres: MasterDirectory: %w", err)
	}
	return email, nil
}
