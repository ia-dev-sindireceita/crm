package postgres_test

// SIN-62878 / Fase 2.5 C4: integration tests for MasterGrantStore and
// MonthlyAllocatorStore against a real Postgres. Lives in the parent
// postgres_test package (mastersession pattern — see wallet_adapter_test.go
// for the rationale on shared-cluster ALTER ROLE race avoidance).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	walletadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres/wallet"
	"github.com/pericles-luz/crm/internal/wallet"
)

// freshDBWithBillingAndTrigger applies migrations 0004, 0005, 0089, 0090,
// and 0097 in order, creating all wallet + billing tables in a clean DB
// that the test exclusively owns.
func freshDBWithBillingAndTrigger(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0089_wallet_basic.up.sql",
		"0090_wallet_updated_at_trigger.up.sql",
		"0097_subscription_plan_invoice_master_grant.up.sql",
	} {
		path := filepath.Join(harness.MigrationsDir(), name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := db.AdminPool().Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	return db
}

// seedTenantAndUser inserts a tenant + master user row via AdminPool.
func seedTenantAndUser(t *testing.T, ctx context.Context, db *testpg.DB) (tenantID, masterID uuid.UUID) {
	t.Helper()
	tenantID = uuid.New()
	masterID = uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantID, "mg-test", fmt.Sprintf("mg-%s.crm.local", tenantID),
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// Master users have tenant_id IS NULL (users_master_xor_tenant CHECK).
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, is_master)
		 VALUES ($1, NULL, $2, 'x', true)`,
		masterID, fmt.Sprintf("master+%s@test.local", masterID),
	); err != nil {
		t.Fatalf("seed master user: %v", err)
	}
	return tenantID, masterID
}

// seedWallet inserts a token_wallet via AdminPool and returns its id.
func seedWalletForTenant(t *testing.T, ctx context.Context, db *testpg.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	var walletID uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`INSERT INTO token_wallet (tenant_id, balance, reserved, version)
		 VALUES ($1, 0, 0, 0) RETURNING id`, tenantID,
	).Scan(&walletID); err != nil {
		t.Fatalf("seed wallet: %v", err)
	}
	return walletID
}

// --- MasterGrantStore tests ------------------------------------------

func TestMasterGrantStore_CreateAndGetByID(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithBillingAndTrigger(t)
	ctx := context.Background()
	tenantID, masterID := seedTenantAndUser(t, ctx, db)

	store, err := walletadapter.NewMasterGrantStore(db.MasterOpsPool(), masterID)
	if err != nil {
		t.Fatalf("NewMasterGrantStore: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	g, err := wallet.NewMasterGrant(
		tenantID, masterID, wallet.KindExtraTokens,
		map[string]any{"tokens": 1000},
		"test grant for integration test",
		now,
	)
	if err != nil {
		t.Fatalf("NewMasterGrant: %v", err)
	}

	if err := store.Create(ctx, g); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.GetByID(ctx, g.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID() != g.ID() {
		t.Errorf("ID = %v, want %v", got.ID(), g.ID())
	}
	if got.ExternalID() != g.ExternalID() {
		t.Errorf("ExternalID = %v, want %v", got.ExternalID(), g.ExternalID())
	}
	if got.TenantID() != tenantID {
		t.Errorf("TenantID = %v, want %v", got.TenantID(), tenantID)
	}
	if got.Kind() != wallet.KindExtraTokens {
		t.Errorf("Kind = %v, want extra_tokens", got.Kind())
	}
	if got.IsRevoked() || got.IsConsumed() {
		t.Error("fresh grant should not be revoked or consumed")
	}
}

func TestMasterGrantStore_ListByTenant(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithBillingAndTrigger(t)
	ctx := context.Background()
	tenantID, masterID := seedTenantAndUser(t, ctx, db)

	store, _ := walletadapter.NewMasterGrantStore(db.MasterOpsPool(), masterID)

	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		g, _ := wallet.NewMasterGrant(tenantID, masterID, wallet.KindExtraTokens, nil,
			fmt.Sprintf("grant %d reason text", i), now)
		if err := store.Create(ctx, g); err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
	}

	list, err := store.ListByTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("ListByTenant returned %d grants, want 3", len(list))
	}
}

func TestMasterGrantStore_ListByTenant_EmptyReturnsSlice(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithBillingAndTrigger(t)
	ctx := context.Background()
	tenantID, masterID := seedTenantAndUser(t, ctx, db)

	store, _ := walletadapter.NewMasterGrantStore(db.MasterOpsPool(), masterID)
	list, err := store.ListByTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if list == nil {
		t.Error("ListByTenant should return empty slice, not nil")
	}
}

func TestMasterGrantStore_Revoke(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithBillingAndTrigger(t)
	ctx := context.Background()
	tenantID, masterID := seedTenantAndUser(t, ctx, db)

	store, _ := walletadapter.NewMasterGrantStore(db.MasterOpsPool(), masterID)

	now := time.Now().UTC()
	g, _ := wallet.NewMasterGrant(tenantID, masterID, wallet.KindExtraTokens, nil,
		"revoke integration test grant", now)
	if err := store.Create(ctx, g); err != nil {
		t.Fatalf("Create: %v", err)
	}

	revoker := masterID
	if err := store.Revoke(ctx, g.ID(), revoker, "revoked for test reason", now); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	got, _ := store.GetByID(ctx, g.ID())
	if !got.IsRevoked() {
		t.Error("grant should be revoked after Revoke")
	}
}

func TestMasterGrantStore_GetByID_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithBillingAndTrigger(t)
	ctx := context.Background()
	_, masterID := seedTenantAndUser(t, ctx, db)

	store, _ := walletadapter.NewMasterGrantStore(db.MasterOpsPool(), masterID)
	_, err := store.GetByID(ctx, uuid.New())
	if !isWalletErr(err, wallet.ErrNotFound) {
		t.Errorf("GetByID missing: want ErrNotFound, got %v", err)
	}
}

// --- MonthlyAllocatorStore tests -------------------------------------

func TestMonthlyAllocatorStore_AllocateMonthlyQuota_Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithBillingAndTrigger(t)
	ctx := context.Background()
	tenantID, masterID := seedTenantAndUser(t, ctx, db)
	_ = seedWalletForTenant(t, ctx, db, tenantID)

	alloc, err := walletadapter.NewMonthlyAllocatorStore(db.MasterOpsPool(), masterID)
	if err != nil {
		t.Fatalf("NewMonthlyAllocatorStore: %v", err)
	}

	period := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	key := fmt.Sprintf("monthly:%s:2026-05", tenantID)

	// First call must succeed and allocate.
	allocated1, err := alloc.AllocateMonthlyQuota(ctx, tenantID, period, 5000, key)
	if err != nil {
		t.Fatalf("first AllocateMonthlyQuota: %v", err)
	}
	if !allocated1 {
		t.Error("first call: allocated = false, want true")
	}

	// Second call with the same key must be idempotent (no new ledger row).
	allocated2, err := alloc.AllocateMonthlyQuota(ctx, tenantID, period, 5000, key)
	if err != nil {
		t.Fatalf("second AllocateMonthlyQuota: %v", err)
	}
	if allocated2 {
		t.Error("second call: allocated = true, want false (idempotent no-op)")
	}

	// The wallet balance should be 5000, not 10000.
	var balance int64
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT balance FROM token_wallet WHERE tenant_id = $1`, tenantID,
	).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if balance != 5000 {
		t.Errorf("balance = %d, want 5000 (idempotent: only one allocation)", balance)
	}

	// Verify the ledger row has source='monthly_alloc'.
	var source string
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT source FROM token_ledger
		  WHERE tenant_id = $1 AND idempotency_key = $2`,
		tenantID, key,
	).Scan(&source); err != nil {
		t.Fatalf("read ledger source: %v", err)
	}
	if source != "monthly_alloc" {
		t.Errorf("ledger source = %q, want monthly_alloc", source)
	}
}

func TestMonthlyAllocatorStore_AllocateMonthlyQuota_NoWallet(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithBillingAndTrigger(t)
	ctx := context.Background()
	tenantID, masterID := seedTenantAndUser(t, ctx, db)
	// Deliberately skip seedWalletForTenant.

	alloc, _ := walletadapter.NewMonthlyAllocatorStore(db.MasterOpsPool(), masterID)
	period := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	_, err := alloc.AllocateMonthlyQuota(ctx, tenantID, period, 5000, "monthly:no-wallet")
	if !isWalletErr(err, wallet.ErrNotFound) {
		t.Errorf("want ErrNotFound for missing wallet, got %v", err)
	}
}

func TestMasterGrantStore_NilPool(t *testing.T) {
	_, err := walletadapter.NewMasterGrantStore(nil, uuid.New())
	if !isPostgresErr(err, postgresadapter.ErrNilPool) {
		t.Errorf("want ErrNilPool, got %v", err)
	}
}

func TestMasterGrantStore_ZeroActor(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithBillingAndTrigger(t)
	_, err := walletadapter.NewMasterGrantStore(db.MasterOpsPool(), uuid.Nil)
	if !isPostgresErr(err, postgresadapter.ErrZeroActor) {
		t.Errorf("want ErrZeroActor, got %v", err)
	}
}

func TestMonthlyAllocatorStore_NilPool(t *testing.T) {
	_, err := walletadapter.NewMonthlyAllocatorStore(nil, uuid.New())
	if !isPostgresErr(err, postgresadapter.ErrNilPool) {
		t.Errorf("want ErrNilPool, got %v", err)
	}
}

// isWalletErr checks errors.Is against a wallet sentinel.
func isWalletErr(err, target error) bool {
	if err == nil || target == nil {
		return err == target
	}
	// wallet errors are plain sentinel vars — use string comparison as
	// fallback when the chain is opaque.
	for e := err; e != nil; {
		if e == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		uw, ok := e.(unwrapper)
		if !ok {
			break
		}
		e = uw.Unwrap()
	}
	return false
}

// isPostgresErr checks errors.Is against a postgres adapter sentinel.
func isPostgresErr(err, target error) bool {
	return isWalletErr(err, target)
}
