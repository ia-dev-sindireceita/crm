package wallet

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// MasterGrantRepository is the persistence port for master_grant rows.
// Implementations MUST run under the master_ops connection because
// app_runtime has SELECT-only access to master_grant (migration 0097).
//
// Error translations required by all implementations:
//
//   - "no rows"                       → ErrNotFound
//   - unique violation on external_id → ErrIdempotencyConflict
type MasterGrantRepository interface {
	// Create persists a new MasterGrant. The grant's ExternalID (ULID)
	// must already be set by the caller (NewMasterGrant does this).
	Create(ctx context.Context, g *MasterGrant) error

	// GetByID returns the MasterGrant with the given internal UUID, or
	// ErrNotFound.
	GetByID(ctx context.Context, id uuid.UUID) (*MasterGrant, error)

	// ListByTenant returns all grants for tenantID ordered by
	// created_at DESC. Returns an empty slice (not ErrNotFound) when
	// none exist.
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*MasterGrant, error)

	// Revoke persists the revocation fields (revoked_at,
	// revoked_by_user_id, revoke_reason) for the grant identified by
	// id. The implementation MUST only update a row that currently has
	// both revoked_at IS NULL and consumed_at IS NULL; if the row is
	// already consumed it returns ErrGrantAlreadyConsumed; if already
	// revoked it returns ErrGrantAlreadyRevoked.
	Revoke(ctx context.Context, id, revokedByUserID uuid.UUID, revokeReason string, now time.Time) error
}

// MonthlyAllocator is the port for the monthly token-quota crediting
// operation. AllocateMonthlyQuota is idempotent by (tenantID,
// periodStart): two calls with the same pair write exactly one
// token_ledger row (source='monthly_alloc').
//
// The implementation runs under the master_ops connection because it
// writes to token_wallet (UPDATE balance) and token_ledger; it does
// not create a master_grant row.
type MonthlyAllocator interface {
	// AllocateMonthlyQuota credits amount tokens to tenantID's wallet
	// for the period beginning at periodStart. idempotencyKey is
	// persisted in token_ledger and must be unique per (wallet, key)
	// tuple — callers conventionally use "monthly:{tenantID}:{period}".
	//
	// Returns (true, nil) when the allocation was written for the first
	// time; (false, nil) when a prior call with the same idempotencyKey
	// already landed (idempotent no-op). Returns ErrNotFound when no
	// wallet exists for tenantID.
	AllocateMonthlyQuota(
		ctx context.Context,
		tenantID uuid.UUID,
		periodStart time.Time,
		amount int64,
		idempotencyKey string,
	) (allocated bool, err error)
}
