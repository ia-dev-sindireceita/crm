package wallet

import (
	"context"

	"github.com/google/uuid"
)

// Repository is the persistence port for the wallet aggregate.
//
// Every method that touches the database carries a tenant id so the
// adapter can run inside WithTenant and let Postgres' RLS policy gate
// the read/write. Domain code never bypasses that scope; the
// reconciler's cross-tenant sweep (PR11) goes through a separate
// master-scoped adapter.
//
// Implementations MUST translate:
//
//   - "no rows" → ErrNotFound
//   - unique violation on (wallet_id, idempotency_key) → ErrIdempotencyConflict
//   - version mismatch on the wallet UPDATE → ErrVersionConflict
//
// so domain code can match with errors.Is without importing pgx.
type Repository interface {
	// LoadByTenant returns the wallet for tenantID, or ErrNotFound.
	LoadByTenant(ctx context.Context, tenantID uuid.UUID) (*TokenWallet, error)

	// ApplyWithLock persists the wallet and the supplied ledger
	// entries in a single transaction. The implementation MUST:
	//
	//   1. BEGIN inside WithTenant(tenantID = w.TenantID());
	//   2. SELECT … FOR UPDATE token_wallet WHERE id = w.ID();
	//   3. verify the row's persisted version == w.Version() - 1;
	//   4. INSERT every entry into token_ledger;
	//   5. UPDATE token_wallet (balance, reserved, version) WHERE id = w.ID()
	//      AND version = persistedVersion;
	//   6. COMMIT.
	//
	// On ErrIdempotencyConflict or ErrVersionConflict the transaction
	// rolls back; on success the row's BEFORE UPDATE trigger refreshes
	// updated_at.
	ApplyWithLock(ctx context.Context, w *TokenWallet, entries []LedgerEntry) error

	// LookupByIdempotencyKey returns the stored ledger row for
	// (walletID, idempotencyKey), or ErrNotFound. The use-case calls
	// this on a Reserve/Commit/Release retry to detect "already
	// applied with this key" and short-circuit by returning the
	// existing entry.
	LookupByIdempotencyKey(ctx context.Context, tenantID, walletID uuid.UUID, idempotencyKey string) (LedgerEntry, error)

	// LookupCompletedByExternalRef returns the commit/release ledger
	// row that closes the reservation identified by externalRef, or
	// ErrNotFound when the reservation is still in flight. Used by
	// the use-case to detect "this reservation has already been
	// settled" before attempting Commit/Release with a fresh
	// idempotency key.
	LookupCompletedByExternalRef(ctx context.Context, tenantID, walletID uuid.UUID, externalRef string) (LedgerEntry, error)

	// ListOpenReservations returns every reserve ledger row on the
	// wallet that has no corresponding commit/release row yet. The
	// F37 reconciler walks this list looking for entries older than
	// maxReservationAge. Read-only; takes no locks.
	ListOpenReservations(ctx context.Context, tenantID, walletID uuid.UUID) ([]LedgerEntry, error)
}
