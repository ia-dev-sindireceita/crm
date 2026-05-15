package postgres_test

// Cross-tenant negative test for the wallet adapter — finding #5 from
// SIN-62748. The pre-existing TestLoadByTenant_CrossTenantHidden seeds
// two tenants then only reads tenant A under WithTenant(A); it doesn't
// actually exercise the RLS denial path. This file adds the real
// negative check: confirm that a SELECT keyed by tenant A's wallet id
// under WithTenant(B) returns zero rows.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	walletadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres/wallet"
)

// TestLoadByTenant_CrossTenantBlocked exercises the RLS denial path:
// when a transaction is opened under WithTenant(B), it must not be
// able to see any token_wallet row owned by tenant A. The test seeds
// both tenants' wallets, then runs a count-keyed-by-id query under
// WithTenant(B) and asserts 0.
func TestLoadByTenant_CrossTenantBlocked(t *testing.T) {
	t.Parallel()
	db, tenantA, masterID := freshDBWithWalletTrigger(t)
	ctx := newWalletCtx(t)
	walletAID := seedWalletWithBalance(t, ctx, db, tenantA, masterID, 100)

	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "wallet-B-block", "b-block.crm.local"); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}
	seedWalletWithBalance(t, ctx, db, tenantB, masterID, 7)

	// Open a transaction under WithTenant(B); query for tenant A's
	// wallet row by primary key. RLS gates token_wallet on
	// app.tenant_id, so the result MUST be zero rows.
	var visible int
	err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM token_wallet WHERE id = $1`, walletAID,
		).Scan(&visible)
	})
	if err != nil {
		t.Fatalf("count under WithTenant(B): %v", err)
	}
	if visible != 0 {
		t.Errorf("tenant B saw %d rows for tenant A's wallet id — RLS leak", visible)
	}

	// And the adapter API path: LoadByTenant(B) must not return A's
	// wallet either. (Distinct from the assertion above because the
	// adapter goes through scanWallet and ErrNotFound — a different
	// code path than the raw count.)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	wb, err := repo.LoadByTenant(ctx, tenantB)
	if err != nil {
		t.Fatalf("LoadByTenant(B): %v", err)
	}
	if wb.ID() == walletAID {
		t.Errorf("LoadByTenant(B) returned tenant A's wallet id %s — RLS leak", walletAID)
	}
	if wb.TenantID() != tenantB {
		t.Errorf("LoadByTenant(B).TenantID = %s, want %s", wb.TenantID(), tenantB)
	}
}

// TestLoadByTenant_CrossTenantLedgerBlocked extends the negative check
// to the token_ledger table: a WithTenant(B) reader MUST NOT see any
// ledger rows owned by tenant A. Defense in depth — the wallet and
// the ledger have separate RLS policies, so both need a leak check.
func TestLoadByTenant_CrossTenantLedgerBlocked(t *testing.T) {
	t.Parallel()
	ctx := newWalletCtx(t)
	db, tenantA, masterID := freshDBWithWalletTrigger(t)
	seedWalletWithBalance(t, ctx, db, tenantA, masterID, 100)

	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "wallet-B-ledger", "b-ledger.crm.local"); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}
	seedWalletWithBalance(t, ctx, db, tenantB, masterID, 7)

	// Generate a ledger row for tenant A via the production path.
	repoA, _ := walletadapter.NewRepository(db.RuntimePool())
	wa, err := repoA.LoadByTenant(ctx, tenantA)
	if err != nil {
		t.Fatalf("LoadByTenant(A): %v", err)
	}
	if err := mustReserveOne(ctx, db, tenantA, wa.ID(), 1); err != nil {
		t.Fatalf("seed ledger row for A: %v", err)
	}

	var leaks int
	err = postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM token_ledger WHERE tenant_id = $1`, tenantA,
		).Scan(&leaks)
	})
	if err != nil {
		t.Fatalf("ledger count under WithTenant(B): %v", err)
	}
	if leaks != 0 {
		t.Errorf("tenant B saw %d ledger rows for tenant A — RLS leak", leaks)
	}
}

// mustReserveOne inserts a single reserve ledger row under tenantID
// via the production WithTenant path. Returns any error so the caller
// can attach a useful t.Fatalf message.
func mustReserveOne(ctx context.Context, db *testpg.DB, tenantID, walletID uuid.UUID, amount int64) error {
	return postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO token_ledger
			   (id, wallet_id, tenant_id, kind, amount, idempotency_key, external_ref, occurred_at, created_at)
			 VALUES ($1, $2, $3, 'reserve', $4, $5, $6, now(), now())`,
			uuid.New(), walletID, tenantID, -amount, "rsv-"+uuid.New().String(), uuid.New().String(),
		)
		return err
	})
}
