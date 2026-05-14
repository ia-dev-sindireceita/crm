// Package wallet (adapter) implements wallet.Repository against
// Postgres. Every method routes through WithTenant so RLS gates the
// read/write at the database, and ApplyWithLock takes a row-level
// FOR UPDATE plus an optimistic version check — the two layers
// together implement F30's atomic-reserve guarantee.
//
// PR11 will add a master-scoped variant for the F37 reconciler's
// cross-tenant sweep; this PR keeps the surface to the tenant-scoped
// path that the use-case layer needs.
package wallet

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/wallet"
)

var _ wallet.Repository = (*Repository)(nil)

// Repository is the pgx-backed wallet repository. It wraps the
// app_runtime pool; the pool's role triggers the RLS policies on
// token_wallet and token_ledger so a missing WithTenant scope
// collapses every query to zero rows.
type Repository struct {
	runtimePool *pgxpool.Pool
}

// NewRepository constructs a wallet repository over the runtime pool.
// nil pool is rejected so callers see a fast panic at first use
// rather than a confusing nil-deref later.
func NewRepository(runtime *pgxpool.Pool) (*Repository, error) {
	if runtime == nil {
		return nil, postgresadapter.ErrNilPool
	}
	return &Repository{runtimePool: runtime}, nil
}

// LoadByTenant returns the wallet for tenantID. RLS on token_wallet
// restricts the SELECT to the row whose tenant_id matches
// app.tenant_id (set by WithTenant), so this query inherently scopes
// to the caller's tenant.
func (r *Repository) LoadByTenant(ctx context.Context, tenantID uuid.UUID) (*wallet.TokenWallet, error) {
	if tenantID == uuid.Nil {
		return nil, wallet.ErrZeroTenant
	}
	var w *wallet.TokenWallet
	err := postgresadapter.WithTenant(ctx, r.runtimePool, tenantID, func(tx pgx.Tx) error {
		got, err := scanWallet(tx.QueryRow(ctx, selectWalletByTenant, tenantID))
		if err != nil {
			return err
		}
		w = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return w, nil
}

// ApplyWithLock is the F30 atomic-reserve linchpin. The
// implementation:
//
//  1. opens a tenant-scoped transaction (WithTenant);
//  2. SELECT … FOR UPDATE the matching token_wallet row;
//  3. checks that the persisted version equals w.Version() - 1
//     (the aggregate has already incremented in memory);
//  4. INSERTs every ledger entry — a 23505 unique-violation on
//     (wallet_id, idempotency_key) is mapped to ErrIdempotencyConflict;
//  5. UPDATEs token_wallet (balance, reserved, version);
//  6. COMMITs.
//
// The BEFORE UPDATE trigger added in migration 0090 refreshes
// updated_at on its own.
func (r *Repository) ApplyWithLock(ctx context.Context, w *wallet.TokenWallet, entries []wallet.LedgerEntry) error {
	if w == nil {
		return fmt.Errorf("wallet/postgres: wallet is nil")
	}
	return postgresadapter.WithTenant(ctx, r.runtimePool, w.TenantID(), func(tx pgx.Tx) error {
		var persistedVersion int64
		err := tx.QueryRow(ctx,
			`SELECT version FROM token_wallet WHERE id = $1 FOR UPDATE`, w.ID(),
		).Scan(&persistedVersion)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return wallet.ErrNotFound
			}
			return fmt.Errorf("wallet/postgres: select for update: %w", err)
		}
		if persistedVersion != w.Version()-1 {
			return wallet.ErrVersionConflict
		}
		for _, e := range entries {
			if _, err := tx.Exec(ctx, insertLedger,
				e.ID, e.WalletID, e.TenantID, string(e.Kind), e.Amount,
				e.IdempotencyKey, nullIfEmpty(e.ExternalRef), e.OccurredAt, e.CreatedAt,
			); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" {
					return wallet.ErrIdempotencyConflict
				}
				return fmt.Errorf("wallet/postgres: insert ledger: %w", err)
			}
		}
		ct, err := tx.Exec(ctx,
			`UPDATE token_wallet
			    SET balance = $1, reserved = $2, version = $3
			  WHERE id = $4 AND version = $5`,
			w.Balance(), w.Reserved(), w.Version(), w.ID(), persistedVersion,
		)
		if err != nil {
			return fmt.Errorf("wallet/postgres: update wallet: %w", err)
		}
		if ct.RowsAffected() != 1 {
			// FOR UPDATE serialised the read, so a missing row here
			// means a concurrent caller updated the version between
			// our SELECT … FOR UPDATE and the UPDATE — which
			// shouldn't happen, but if it does we surface as a
			// version conflict so the use-case retries.
			return wallet.ErrVersionConflict
		}
		return nil
	})
}

// LookupByIdempotencyKey returns the ledger row with the given
// idempotency key on this wallet, or ErrNotFound.
func (r *Repository) LookupByIdempotencyKey(ctx context.Context, tenantID, walletID uuid.UUID, idempotencyKey string) (wallet.LedgerEntry, error) {
	if tenantID == uuid.Nil || walletID == uuid.Nil || idempotencyKey == "" {
		return wallet.LedgerEntry{}, wallet.ErrNotFound
	}
	var out wallet.LedgerEntry
	err := postgresadapter.WithTenant(ctx, r.runtimePool, tenantID, func(tx pgx.Tx) error {
		entry, err := scanLedger(tx.QueryRow(ctx, selectLedgerByIdem, walletID, idempotencyKey))
		if err != nil {
			return err
		}
		out = entry
		return nil
	})
	if err != nil {
		return wallet.LedgerEntry{}, err
	}
	return out, nil
}

// LookupCompletedByExternalRef returns the commit/release row that
// settled the reservation identified by externalRef, or ErrNotFound
// if the reservation is still open.
func (r *Repository) LookupCompletedByExternalRef(ctx context.Context, tenantID, walletID uuid.UUID, externalRef string) (wallet.LedgerEntry, error) {
	if tenantID == uuid.Nil || walletID == uuid.Nil || externalRef == "" {
		return wallet.LedgerEntry{}, wallet.ErrNotFound
	}
	var out wallet.LedgerEntry
	err := postgresadapter.WithTenant(ctx, r.runtimePool, tenantID, func(tx pgx.Tx) error {
		entry, err := scanLedger(tx.QueryRow(ctx, selectCompletedByExternalRef, walletID, externalRef))
		if err != nil {
			return err
		}
		out = entry
		return nil
	})
	if err != nil {
		return wallet.LedgerEntry{}, err
	}
	return out, nil
}

// ListOpenReservations returns every reserve ledger row on the wallet
// that has not been settled by a commit/release row sharing its
// external_ref. Read-only; takes no locks.
func (r *Repository) ListOpenReservations(ctx context.Context, tenantID, walletID uuid.UUID) ([]wallet.LedgerEntry, error) {
	if tenantID == uuid.Nil || walletID == uuid.Nil {
		return nil, nil
	}
	var out []wallet.LedgerEntry
	err := postgresadapter.WithTenant(ctx, r.runtimePool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, selectOpenReservations, walletID)
		if err != nil {
			return fmt.Errorf("wallet/postgres: list open reservations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			entry, err := scanLedgerRow(rows)
			if err != nil {
				return err
			}
			out = append(out, entry)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// nullIfEmpty turns the empty string into a NULL parameter for pgx.
// Used for external_ref, which is NULLABLE in the database.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
