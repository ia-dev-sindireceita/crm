package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MasterCredentialReader satisfies iam.MasterCredentialReader. It reads the
// (id, password_hash) of the GLOBAL master operator keyed by email, scoped
// to is_master = true AND tenant_id IS NULL, using the app_master_ops role
// via WithMasterOps — the same tenant-less master read path as
// MasterDirectory. The runtime (app_runtime) pool cannot see NULL-tenant
// master rows because RLS filters by app.current_tenant; the master-ops
// role + GUC is the sanctioned cross-tenant read.
type MasterCredentialReader struct {
	pool    TxBeginner
	actorID uuid.UUID
}

// NewMasterCredentialReader returns the adapter. Nil pool or zero actorID
// return errors consistent with the rest of the postgres package.
func NewMasterCredentialReader(pool *pgxpool.Pool, actorID uuid.UUID) (*MasterCredentialReader, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	if actorID == uuid.Nil {
		return nil, ErrZeroActor
	}
	return &MasterCredentialReader{pool: pool, actorID: actorID}, nil
}

// LookupMasterCredentials returns the id + encoded password hash of the
// master operator whose email matches (case-insensitively). When no master
// row matches it returns (uuid.Nil, "", nil) — the zero-id sentinel the
// iam.MasterCredentialReader contract requires so the use-case can run its
// timing-equivalent dummy verify on the miss path rather than treating a
// miss as an infrastructure error.
//
// The lookup is pinned to is_master = true AND tenant_id IS NULL so a
// tenant user row (even one with a mis-typed role='master', which the
// users table allows — no CHECK constraint, SIN-63340) can never be
// resolved as the master operator: tenant rows carry a non-NULL tenant_id.
func (r *MasterCredentialReader) LookupMasterCredentials(ctx context.Context, email string) (uuid.UUID, string, error) {
	var (
		id   uuid.UUID
		hash string
	)
	err := WithMasterOps(ctx, r.pool, r.actorID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, password_hash
			   FROM users
			  WHERE lower(email) = lower($1)
			    AND is_master = true
			    AND tenant_id IS NULL`,
			email,
		).Scan(&id, &hash)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", nil
	}
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("postgres: MasterCredentialReader: %w", err)
	}
	return id, hash, nil
}
