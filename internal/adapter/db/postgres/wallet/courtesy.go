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

var _ wallet.CourtesyGrantRepository = (*CourtesyStore)(nil)

// CourtesyStore is the pgx-backed adapter for the SIN-62730
// courtesy-grant flow. The store wraps the app_master_ops pool
// because courtesy_grant is INSERTable only by master_ops (migration
// 0089); running the entire 4-statement transaction under
// WithMasterOps lets the same audit chain cover the wallet bootstrap
// too. ADR 0093 explains the role split.
//
// The adapter is its own type — separate from wallet.Repository — so
// the runtime path that the Reserve/Commit/Release service uses keeps
// the app_runtime pool, while the rare onboarding path opens its own
// connection on the higher-privilege pool.
type CourtesyStore struct {
	masterOpsPool postgresadapter.TxBeginner
}

// NewCourtesyStore wraps a master_ops pool. The pool MUST log in as
// app_master_ops so the master_ops_audit_trigger receives the GUC
// app.master_ops_actor_user_id set by WithMasterOps. A nil pool is
// rejected so callers see a fast panic at construction.
func NewCourtesyStore(masterOps *pgxpool.Pool) (*CourtesyStore, error) {
	if masterOps == nil {
		return nil, postgresadapter.ErrNilPool
	}
	return &CourtesyStore{masterOpsPool: masterOps}, nil
}

// Issue runs the four onboarding statements in a single master_ops
// transaction:
//
//  1. INSERT INTO courtesy_grant (tenant_id, amount, granted_by_user_id)
//     ON CONFLICT (tenant_id) DO NOTHING RETURNING id
//  2. INSERT INTO token_wallet (tenant_id, balance=0, reserved=0, version=0) RETURNING id
//  3. INSERT INTO token_ledger (wallet_id, tenant_id, kind='grant',
//     amount, idempotency_key='courtesy:'||tenantID, external_ref=grantID, …)
//  4. UPDATE token_wallet SET balance = $amount, version = 1 WHERE id = $walletID
//
// Step 1 is the idempotency choke point: courtesy_grant has UNIQUE
// (tenant_id), so concurrent calls for the same tenantID serialize on
// the partial-unique conflict. The loser's ON CONFLICT DO NOTHING
// returns pgx.ErrNoRows, the adapter swallows it and re-reads the
// committed grant + wallet, returning Issued{Granted: false}.
//
// Steps 2-4 only run for the winner. The defense-in-depth UNIQUE on
// token_ledger (wallet_id, idempotency_key) means even if a future
// caller reaches step 3 with a stale state, the grant ledger row
// cannot be inserted twice.
func (s *CourtesyStore) Issue(ctx context.Context, tenantID, actorID uuid.UUID, amount int64) (wallet.Issued, error) {
	if tenantID == uuid.Nil {
		return wallet.Issued{}, wallet.ErrZeroTenant
	}
	if actorID == uuid.Nil {
		return wallet.Issued{}, postgresadapter.ErrZeroActor
	}
	if amount <= 0 {
		return wallet.Issued{}, wallet.ErrInvalidAmount
	}

	var out wallet.Issued
	err := postgresadapter.WithMasterOps(ctx, s.masterOpsPool, actorID, func(tx pgx.Tx) error {
		// Step 1 — claim the per-tenant slot in courtesy_grant.
		var grantID uuid.UUID
		insertErr := tx.QueryRow(ctx,
			`INSERT INTO courtesy_grant (tenant_id, amount, granted_by_user_id)
			      VALUES ($1, $2, $3)
			 ON CONFLICT (tenant_id) DO NOTHING
			 RETURNING id`,
			tenantID, amount, actorID,
		).Scan(&grantID)

		if errors.Is(insertErr, pgx.ErrNoRows) {
			// Already granted. Read the surviving rows and report a
			// silent no-op so the caller can keep going.
			var existingGrant, existingWallet uuid.UUID
			err := tx.QueryRow(ctx,
				`SELECT g.id, w.id
				   FROM courtesy_grant g
				   JOIN token_wallet  w ON w.tenant_id = g.tenant_id
				  WHERE g.tenant_id = $1`,
				tenantID,
			).Scan(&existingGrant, &existingWallet)
			if err != nil {
				return fmt.Errorf("wallet/postgres: load existing courtesy grant: %w", err)
			}
			out = wallet.Issued{Granted: false, GrantID: existingGrant, WalletID: existingWallet}
			return nil
		}
		if insertErr != nil {
			return fmt.Errorf("wallet/postgres: insert courtesy grant: %w", insertErr)
		}

		// Step 2 — create the wallet at balance=0; the grant ledger
		// row in step 3 records the credit, the UPDATE in step 4
		// materializes the balance.
		var walletID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO token_wallet (tenant_id, balance, reserved, version)
			      VALUES ($1, 0, 0, 0)
			 RETURNING id`,
			tenantID,
		).Scan(&walletID); err != nil {
			return fmt.Errorf("wallet/postgres: insert wallet: %w", err)
		}

		// Step 3 — record the grant in the ledger with the canonical
		// idempotency_key. SignedAmount keeps the sign convention in
		// one place (Grant is positive).
		now := time.Now().UTC()
		ledgerID := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO token_ledger
			  (id, wallet_id, tenant_id, kind, amount, idempotency_key, external_ref, occurred_at, created_at)
			 VALUES ($1, $2, $3, 'grant', $4, $5, $6, $7, $7)`,
			ledgerID, walletID, tenantID,
			wallet.SignedAmount(wallet.KindGrant, amount),
			courtesyIdempotencyKey(tenantID), grantID.String(), now,
		); err != nil {
			return fmt.Errorf("wallet/postgres: insert ledger grant: %w", err)
		}

		// Step 4 — materialize the balance. version=1 mirrors what
		// wallet.New + Grant would have produced via the use-case
		// path, so subsequent Reserve/Commit see a consistent stamp.
		ct, err := tx.Exec(ctx,
			`UPDATE token_wallet SET balance = $1, version = 1
			  WHERE id = $2 AND version = 0`,
			amount, walletID,
		)
		if err != nil {
			return fmt.Errorf("wallet/postgres: update wallet balance: %w", err)
		}
		if ct.RowsAffected() != 1 {
			return fmt.Errorf("wallet/postgres: courtesy update wallet balance affected %d rows", ct.RowsAffected())
		}

		out = wallet.Issued{Granted: true, GrantID: grantID, WalletID: walletID}
		return nil
	})
	if err != nil {
		return wallet.Issued{}, err
	}
	return out, nil
}

// courtesyIdempotencyKey is the canonical key written into
// token_ledger.idempotency_key for the on-tenant-creation grant.
// Exposed via a helper so test assertions and (future) reconciliation
// paths use the exact same string.
func courtesyIdempotencyKey(tenantID uuid.UUID) string {
	return "courtesy:" + tenantID.String()
}

// CourtesyIdempotencyKey is the package-public alias for callers that
// need to construct or assert the same key (e.g. integration tests,
// reconciler queries).
func CourtesyIdempotencyKey(tenantID uuid.UUID) string {
	return courtesyIdempotencyKey(tenantID)
}
