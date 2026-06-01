package wallet

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pericles-luz/crm/internal/wallet"
)

// Hot-path queries are package-level constants so go/types can keep
// them in the read-only data segment and pgx's prepared-statement
// cache hits on identical strings.
const (
	selectWalletByTenant = `
		SELECT id, tenant_id, balance, reserved, version, created_at, updated_at
		  FROM token_wallet
		 WHERE tenant_id = $1
	`

	// insertLedger is the pre-SIN-62936 INSERT used by every
	// reserve/commit/release/non-master-grant flow. It omits the
	// migration-0097 `source` + `master_grant_id` columns so this
	// adapter remains backwards-compatible with deploys (and test
	// fixtures) that have not yet applied 0097. Those columns get
	// the table's DEFAULT (source='consumption', master_grant_id NULL).
	insertLedger = `
		INSERT INTO token_ledger
		  (id, wallet_id, tenant_id, kind, amount, idempotency_key, external_ref, occurred_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`

	// insertLedgerWithSource is the post-SIN-62936 INSERT used when
	// the caller supplies a non-default Source on the ledger entry
	// (currently: only the master-grant applier, with source =
	// 'master_grant' + master_grant_id set). Callers that hit this
	// path implicitly require migration 0097's `source` +
	// `master_grant_id` columns; the same UNIQUE (wallet_id,
	// idempotency_key) → ErrIdempotencyConflict translation applies.
	insertLedgerWithSource = `
		INSERT INTO token_ledger
		  (id, wallet_id, tenant_id, kind, amount, idempotency_key, external_ref,
		   source, master_grant_id, occurred_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`

	selectLedgerByIdem = `
		SELECT id, wallet_id, tenant_id, kind, amount, idempotency_key,
		       COALESCE(external_ref, ''), occurred_at, created_at
		  FROM token_ledger
		 WHERE wallet_id = $1 AND idempotency_key = $2
	`

	// Settled = kind ∈ {commit, release} for the same external_ref.
	selectCompletedByExternalRef = `
		SELECT id, wallet_id, tenant_id, kind, amount, idempotency_key,
		       COALESCE(external_ref, ''), occurred_at, created_at
		  FROM token_ledger
		 WHERE wallet_id = $1
		   AND external_ref = $2
		   AND kind IN ('commit', 'release')
		 LIMIT 1
	`

	// Open reservation = reserve row whose external_ref has no
	// matching commit/release row on the same wallet. Anti-join
	// against token_ledger keeps the read read-only.
	selectOpenReservations = `
		SELECT r.id, r.wallet_id, r.tenant_id, r.kind, r.amount,
		       r.idempotency_key, COALESCE(r.external_ref, ''),
		       r.occurred_at, r.created_at
		  FROM token_ledger r
		 WHERE r.wallet_id = $1
		   AND r.kind = 'reserve'
		   AND NOT EXISTS (
		         SELECT 1 FROM token_ledger s
		          WHERE s.wallet_id = r.wallet_id
		            AND s.external_ref = r.external_ref
		            AND s.kind IN ('commit', 'release')
		       )
		 ORDER BY r.occurred_at ASC
	`
)

// rowScanner is the minimal pgx surface scanWallet / scanLedger need.
// Defining it lets the same helper handle pgx.Row (QueryRow) and the
// per-row .Scan on a pgx.Rows iterator.
type rowScanner interface {
	Scan(dest ...any) error
}

func (r *Repository) scanWallet(s rowScanner) (*wallet.TokenWallet, error) {
	var (
		id, tenantID               uuid.UUID
		balance, reserved, version int64
		createdAt, updatedAt       time.Time
	)
	if err := s.Scan(&id, &tenantID, &balance, &reserved, &version, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, wallet.ErrNotFound
		}
		return nil, err
	}
	return r.hydrator.Hydrate(id, tenantID, balance, reserved, version, createdAt, updatedAt), nil
}

func scanLedger(s rowScanner) (wallet.LedgerEntry, error) {
	entry, err := decodeLedger(s.Scan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return wallet.LedgerEntry{}, wallet.ErrNotFound
		}
		return wallet.LedgerEntry{}, err
	}
	return entry, nil
}

// scanLedgerRow wraps the Scan from a pgx.Rows iterator step so the
// ListOpenReservations loop does not double-wrap pgx.ErrNoRows (which
// never fires on Rows.Scan anyway).
func scanLedgerRow(rows pgx.Rows) (wallet.LedgerEntry, error) {
	return decodeLedger(rows.Scan)
}

// decodeLedger is the shared scan logic; both row and rows callers
// hand it a Scan closure to keep the column order in one place.
// Source + MasterGrantID on the returned LedgerEntry are left at the
// zero value — callers that need those columns read the underlying
// token_ledger row directly (none in the existing read paths). When
// the SELECT shape evolves to include them, scan order needs to be
// updated together with insertLedgerWithSource.
func decodeLedger(scan func(dest ...any) error) (wallet.LedgerEntry, error) {
	var (
		id, walletID, tenantID uuid.UUID
		kind, idempotencyKey   string
		externalRef            string
		amount                 int64
		occurredAt, createdAt  time.Time
	)
	if err := scan(&id, &walletID, &tenantID, &kind, &amount, &idempotencyKey, &externalRef, &occurredAt, &createdAt); err != nil {
		return wallet.LedgerEntry{}, err
	}
	return wallet.LedgerEntry{
		ID:             id,
		WalletID:       walletID,
		TenantID:       tenantID,
		Kind:           wallet.LedgerKind(kind),
		Amount:         amount,
		IdempotencyKey: idempotencyKey,
		ExternalRef:    externalRef,
		OccurredAt:     occurredAt,
		CreatedAt:      createdAt,
	}, nil
}
