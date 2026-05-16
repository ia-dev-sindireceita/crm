package wallet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/wallet"
)

var _ wallet.MonthlyAllocator = (*MonthlyAllocatorStore)(nil)

// MonthlyAllocatorStore implements wallet.MonthlyAllocator against Postgres.
// AllocateMonthlyQuota runs under WithMasterOps because it writes to
// token_wallet (UPDATE balance) which requires the master_ops role for
// cross-tenant writes. The runtime pool cannot UPDATE token_wallet for a
// different tenant's scope.
type MonthlyAllocatorStore struct {
	pool    postgresadapter.TxBeginner
	actorID uuid.UUID
}

// NewMonthlyAllocatorStore constructs the allocator over the master_ops pool.
func NewMonthlyAllocatorStore(masterOps *pgxpool.Pool, actorID uuid.UUID) (*MonthlyAllocatorStore, error) {
	if masterOps == nil {
		return nil, postgresadapter.ErrNilPool
	}
	if actorID == uuid.Nil {
		return nil, postgresadapter.ErrZeroActor
	}
	return &MonthlyAllocatorStore{pool: masterOps, actorID: actorID}, nil
}

// AllocateMonthlyQuota credits amount tokens to tenantID's wallet for the
// period beginning at periodStart. Idempotent: a second call with the same
// idempotencyKey returns (false, nil) without writing a second ledger row.
//
// The implementation:
//  1. SELECTs the wallet for tenantID (fails with ErrNotFound if missing).
//  2. INSERTs a token_ledger row with source='monthly_alloc' and the
//     supplied idempotencyKey. A unique violation means a prior call
//     already landed — return (false, nil).
//  3. UPDATEs token_wallet SET balance += amount.
//
// All three steps run inside a single WithMasterOps transaction so the
// ledger row and wallet balance are always in sync.
func (s *MonthlyAllocatorStore) AllocateMonthlyQuota(
	ctx context.Context,
	tenantID uuid.UUID,
	periodStart time.Time,
	amount int64,
	idempotencyKey string,
) (bool, error) {
	if tenantID == uuid.Nil {
		return false, wallet.ErrZeroTenant
	}
	if amount <= 0 {
		return false, wallet.ErrInvalidAmount
	}
	if idempotencyKey == "" {
		return false, wallet.ErrEmptyIdempotencyKey
	}

	var allocated bool
	err := postgresadapter.WithMasterOps(ctx, s.pool, s.actorID, func(tx pgx.Tx) error {
		// Step 1: load wallet (SELECT FOR UPDATE to guard balance).
		var walletID uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT id FROM token_wallet WHERE tenant_id = $1 FOR UPDATE`,
			tenantID,
		).Scan(&walletID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return wallet.ErrNotFound
			}
			return fmt.Errorf("wallet/postgres: load wallet for monthly alloc: %w", err)
		}

		// Step 2: insert ledger row (idempotency guard via ON CONFLICT).
		// Using ON CONFLICT DO NOTHING rather than relying on the pgx
		// error path: a plain Exec error aborts the pgx transaction and
		// we cannot continue in the same TxFunc — the commit would
		// return "commit unexpectedly resulted in rollback".
		now := time.Now().UTC()
		ledgerID := uuid.New()
		ct, err := tx.Exec(ctx, `
			INSERT INTO token_ledger
			  (id, wallet_id, tenant_id, kind, amount,
			   idempotency_key, occurred_at, created_at, source)
			VALUES ($1,$2,$3,'grant',$4,$5,$6,$6,'monthly_alloc')
			ON CONFLICT (wallet_id, idempotency_key)
			WHERE wallet_id IS NOT NULL
			DO NOTHING`,
			ledgerID, walletID, tenantID,
			wallet.SignedAmount(wallet.KindGrant, amount),
			idempotencyKey, now,
		)
		if err != nil {
			return fmt.Errorf("wallet/postgres: insert monthly_alloc ledger: %w", err)
		}
		if ct.RowsAffected() == 0 {
			// Idempotent no-op — prior call already landed.
			allocated = false
			return nil
		}

		// Step 3: materialise the balance credit.
		uct, uerr := tx.Exec(ctx,
			`UPDATE token_wallet SET balance = balance + $1, version = version + 1
			  WHERE id = $2`,
			amount, walletID,
		)
		if uerr != nil {
			return fmt.Errorf("wallet/postgres: update wallet balance for monthly alloc: %w", uerr)
		}
		if uct.RowsAffected() != 1 {
			return fmt.Errorf("wallet/postgres: monthly alloc balance update affected %d rows", uct.RowsAffected())
		}
		_ = periodStart // carried in idempotencyKey; kept as a parameter for callers
		allocated = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return allocated, nil
}
