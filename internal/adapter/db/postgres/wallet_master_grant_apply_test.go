package postgres_test

// SIN-62936 — integration tests for the master_grant downstream
// applier (ApplyMasterGrantService). Two scenarios cover the
// parent C10 ticket's CA #2 and CA #3:
//
//   CA #2 — master concede 30 dias grátis →
//           Subscription.status=active, current_period_end > now(),
//           NENHUM invoice criado.
//   CA #3 — master concede 1M tokens →
//           ledger_entry.source=master_grant, saldo aumenta 1M,
//           master_grant_id na ledger row aponta para o grant.
//
// Lives in the parent postgres_test package (mastersession pattern —
// shared harness, single TestMain, avoids the ALTER ROLE race on the
// shared CI cluster).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	billingpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/billing"
	walletadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres/wallet"
	"github.com/pericles-luz/crm/internal/billing"
	"github.com/pericles-luz/crm/internal/wallet"
	walletusecase "github.com/pericles-luz/crm/internal/wallet/usecase"
)

// helper: seed an active subscription via master_ops + capture the
// generated id, returning the row's external id for assertion.
func seedActiveSubForApply(t *testing.T, ctx context.Context, store *billingpg.Store, tenantID, planID, masterID uuid.UUID) uuid.UUID {
	t.Helper()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC).UTC()
	sub, err := billing.NewSubscription(tenantID, planID, now, now.Add(30*24*time.Hour), now)
	if err != nil {
		t.Fatalf("NewSubscription: %v", err)
	}
	if err := store.SaveSubscription(ctx, sub, masterID); err != nil {
		t.Fatalf("SaveSubscription: %v", err)
	}
	return sub.ID()
}

// helper: create a fresh master_grant via the audited adapter,
// returning the persisted entity (with the assigned id + external_id).
func createMasterGrant(t *testing.T, ctx context.Context, grants *walletadapter.MasterGrantStore, tenantID, masterID uuid.UUID, kind wallet.MasterGrantKind, payload map[string]any, reason string, createdAt time.Time) *wallet.MasterGrant {
	t.Helper()
	g, err := wallet.NewMasterGrant(tenantID, masterID, kind, payload, reason, createdAt)
	if err != nil {
		t.Fatalf("NewMasterGrant: %v", err)
	}
	if err := grants.Create(ctx, g); err != nil {
		t.Fatalf("MasterGrantStore.Create: %v", err)
	}
	return g
}

// ---------- CA #2: free_subscription_period --------------------------

func TestApplyMasterGrant_Integration_FreePeriod_ExtendsSubscriptionWithoutInvoice(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithBillingAndTrigger(t)
	ctx := context.Background()
	tenantID, masterID := seedTenantAndUser(t, ctx, db)

	// Plan + active subscription for the tenant. The subscription
	// adapter writes via master_ops (audit fires); the ApplyMasterGrant
	// usecase reads via app_runtime RLS, so we exercise the same
	// path the master console will take in production.
	planID := seedPlan(t, ctx, db, "pro-apply-free", 1_000_000)
	billingStore := newBillingStore(t, db)
	subID := seedActiveSubForApply(t, ctx, billingStore, tenantID, planID, masterID)

	// Snapshot the original current_period_end via admin pool so the
	// test does not depend on the runtime read path (which would need
	// a tenanted context).
	var originalEnd time.Time
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT current_period_end FROM subscription WHERE id = $1`, subID).Scan(&originalEnd); err != nil {
		t.Fatalf("read original period end: %v", err)
	}

	grants, err := walletadapter.NewMasterGrantStore(db.MasterOpsPool(), masterID)
	if err != nil {
		t.Fatalf("NewMasterGrantStore: %v", err)
	}
	walletRepo, err := walletadapter.NewRepository(db.RuntimePool())
	if err != nil {
		t.Fatalf("NewRepository(wallet): %v", err)
	}
	now := time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC).UTC()
	applier, err := walletusecase.NewApplyMasterGrantService(grants, walletRepo, billingStore, func() time.Time { return now }, masterID)
	if err != nil {
		t.Fatalf("NewApplyMasterGrantService: %v", err)
	}

	g := createMasterGrant(t, ctx, grants, tenantID, masterID,
		wallet.KindFreeSubscriptionPeriod,
		map[string]any{"period_days": 30},
		"CA#2 — extensão de 30 dias grátis",
		now.Add(-time.Minute),
	)

	applied, err := applier.Apply(ctx, g.ID())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !applied {
		t.Fatal("Apply returned applied=false on fresh grant")
	}

	// AC #1 from this ticket / CA #2 from SIN-62884:
	//   status=active, current_period_end > now(), NO invoice.
	var (
		status   string
		newEnd   time.Time
		invoices int
	)
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT status, current_period_end FROM subscription WHERE id = $1`, subID,
	).Scan(&status, &newEnd); err != nil {
		t.Fatalf("read subscription after apply: %v", err)
	}
	if status != string(billing.SubscriptionStatusActive) {
		t.Errorf("subscription.status = %q, want active", status)
	}
	wantEnd := originalEnd.Add(30 * 24 * time.Hour)
	if !newEnd.Equal(wantEnd) {
		t.Errorf("current_period_end = %s, want %s (+ 30 days)", newEnd, wantEnd)
	}

	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM invoice WHERE tenant_id = $1`, tenantID,
	).Scan(&invoices); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	if invoices != 0 {
		t.Errorf("free period grant created %d invoice(s); want 0", invoices)
	}

	// The grant row must be consumed_at = now + consumed_ref = subscription id.
	var (
		consumedAt  *time.Time
		consumedRef *string
	)
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT consumed_at, consumed_ref FROM master_grant WHERE id = $1`, g.ID(),
	).Scan(&consumedAt, &consumedRef); err != nil {
		t.Fatalf("read grant after apply: %v", err)
	}
	if consumedAt == nil {
		t.Fatal("master_grant.consumed_at remained NULL after apply")
	}
	if consumedRef == nil || *consumedRef != subID.String() {
		t.Errorf("consumed_ref = %v, want %s (subscription id)", consumedRef, subID)
	}
}

// ---------- CA #3: extra_tokens --------------------------------------

func TestApplyMasterGrant_Integration_ExtraTokens_CreditsWalletAndStampsSource(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithBillingAndTrigger(t)
	ctx := context.Background()
	tenantID, masterID := seedTenantAndUser(t, ctx, db)
	_ = seedWalletForTenant(t, ctx, db, tenantID)

	grants, err := walletadapter.NewMasterGrantStore(db.MasterOpsPool(), masterID)
	if err != nil {
		t.Fatalf("NewMasterGrantStore: %v", err)
	}
	walletRepo, err := walletadapter.NewRepository(db.RuntimePool())
	if err != nil {
		t.Fatalf("NewRepository(wallet): %v", err)
	}
	billingStore := newBillingStore(t, db)
	now := time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC).UTC()
	applier, err := walletusecase.NewApplyMasterGrantService(grants, walletRepo, billingStore, func() time.Time { return now }, masterID)
	if err != nil {
		t.Fatalf("NewApplyMasterGrantService: %v", err)
	}

	const amount int64 = 1_000_000
	g := createMasterGrant(t, ctx, grants, tenantID, masterID,
		wallet.KindExtraTokens,
		map[string]any{"amount": amount},
		"CA#3 — concessão de 1M tokens",
		now.Add(-time.Minute),
	)

	applied, err := applier.Apply(ctx, g.ID())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !applied {
		t.Fatal("Apply returned applied=false on fresh grant")
	}

	// Wallet balance increased by 1_000_000.
	var balance int64
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT balance FROM token_wallet WHERE tenant_id = $1`, tenantID,
	).Scan(&balance); err != nil {
		t.Fatalf("read wallet balance: %v", err)
	}
	if balance != amount {
		t.Errorf("balance after apply = %d, want %d", balance, amount)
	}

	// Exactly one ledger row, with source=master_grant + master_grant_id
	// pointing at the grant + Amount=+1_000_000.
	var (
		rowCount      int
		ledgerSource  string
		ledgerAmount  int64
		ledgerKind    string
		masterGrantID *uuid.UUID
		idemKey       string
	)
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM token_ledger WHERE tenant_id = $1`, tenantID,
	).Scan(&rowCount); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("ledger row count = %d, want 1", rowCount)
	}
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT source, amount, kind, master_grant_id, idempotency_key
		   FROM token_ledger WHERE tenant_id = $1`, tenantID,
	).Scan(&ledgerSource, &ledgerAmount, &ledgerKind, &masterGrantID, &idemKey); err != nil {
		t.Fatalf("read ledger row: %v", err)
	}
	if ledgerSource != string(wallet.SourceMasterGrant) {
		t.Errorf("ledger.source = %q, want master_grant", ledgerSource)
	}
	if ledgerAmount != amount {
		t.Errorf("ledger.amount = %d, want %d (positive grant)", ledgerAmount, amount)
	}
	if ledgerKind != string(wallet.KindGrant) {
		t.Errorf("ledger.kind = %q, want grant", ledgerKind)
	}
	if masterGrantID == nil || *masterGrantID != g.ID() {
		t.Errorf("ledger.master_grant_id = %v, want %s", masterGrantID, g.ID())
	}
	if idemKey != "master_grant:"+g.ExternalID() {
		t.Errorf("ledger.idempotency_key = %q, want master_grant:%s", idemKey, g.ExternalID())
	}

	// Grant row consumed_at + consumed_ref pointing at the ledger row.
	var (
		consumedAt  *time.Time
		consumedRef *string
		ledgerID    uuid.UUID
	)
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT consumed_at, consumed_ref FROM master_grant WHERE id = $1`, g.ID(),
	).Scan(&consumedAt, &consumedRef); err != nil {
		t.Fatalf("read grant after apply: %v", err)
	}
	if consumedAt == nil {
		t.Fatal("master_grant.consumed_at remained NULL after apply")
	}
	if consumedRef == nil {
		t.Fatal("master_grant.consumed_ref remained NULL after apply")
	}
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT id FROM token_ledger WHERE tenant_id = $1`, tenantID,
	).Scan(&ledgerID); err != nil {
		t.Fatalf("read ledger id: %v", err)
	}
	if *consumedRef != ledgerID.String() {
		t.Errorf("consumed_ref = %s, want ledger id %s", *consumedRef, ledgerID)
	}
}

// ---------- idempotency (CA #4 + #5 framing) -------------------------

func TestApplyMasterGrant_Integration_RerunIsNoOp(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithBillingAndTrigger(t)
	ctx := context.Background()
	tenantID, masterID := seedTenantAndUser(t, ctx, db)
	_ = seedWalletForTenant(t, ctx, db, tenantID)

	grants, err := walletadapter.NewMasterGrantStore(db.MasterOpsPool(), masterID)
	if err != nil {
		t.Fatalf("NewMasterGrantStore: %v", err)
	}
	walletRepo, err := walletadapter.NewRepository(db.RuntimePool())
	if err != nil {
		t.Fatalf("NewRepository(wallet): %v", err)
	}
	billingStore := newBillingStore(t, db)
	now := time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC).UTC()
	applier, err := walletusecase.NewApplyMasterGrantService(grants, walletRepo, billingStore, func() time.Time { return now }, masterID)
	if err != nil {
		t.Fatalf("NewApplyMasterGrantService: %v", err)
	}

	const amount int64 = 250_000
	g := createMasterGrant(t, ctx, grants, tenantID, masterID,
		wallet.KindExtraTokens,
		map[string]any{"amount": amount},
		"idempotency integration test",
		now.Add(-time.Minute),
	)

	if _, err := applier.Apply(ctx, g.ID()); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	applied2, err := applier.Apply(ctx, g.ID())
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if applied2 {
		t.Error("second Apply on consumed grant returned applied=true; want false (no-op)")
	}

	var rowCount int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM token_ledger WHERE tenant_id = $1`, tenantID,
	).Scan(&rowCount); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("ledger rows after re-apply = %d, want 1 (idempotency)", rowCount)
	}
	var balance int64
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT balance FROM token_wallet WHERE tenant_id = $1`, tenantID,
	).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if balance != amount {
		t.Errorf("balance after re-apply = %d, want %d (no double credit)", balance, amount)
	}
}

// SIN-63901 — free_period analog of RerunIsNoOp. Two consecutive
// Apply calls on the same free_subscription_period grant must extend
// current_period_end exactly once. The first call lands the
// extension and marks consumed_at; the second observes
// consumed_at != nil (top-of-Apply IsConsumed check fires first;
// applyFreePeriod's pre-check would catch the same state if the
// initial check raced past it) and returns (false, nil) without
// touching the subscription.
func TestApplyMasterGrant_Integration_FreePeriod_RerunIsNoOp(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithBillingAndTrigger(t)
	ctx := context.Background()
	tenantID, masterID := seedTenantAndUser(t, ctx, db)

	planID := seedPlan(t, ctx, db, "pro-apply-free-rerun", 1_000_000)
	billingStore := newBillingStore(t, db)
	subID := seedActiveSubForApply(t, ctx, billingStore, tenantID, planID, masterID)

	var originalEnd time.Time
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT current_period_end FROM subscription WHERE id = $1`, subID,
	).Scan(&originalEnd); err != nil {
		t.Fatalf("read original period end: %v", err)
	}

	grants, err := walletadapter.NewMasterGrantStore(db.MasterOpsPool(), masterID)
	if err != nil {
		t.Fatalf("NewMasterGrantStore: %v", err)
	}
	walletRepo, err := walletadapter.NewRepository(db.RuntimePool())
	if err != nil {
		t.Fatalf("NewRepository(wallet): %v", err)
	}
	now := time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC).UTC()
	applier, err := walletusecase.NewApplyMasterGrantService(grants, walletRepo, billingStore, func() time.Time { return now }, masterID)
	if err != nil {
		t.Fatalf("NewApplyMasterGrantService: %v", err)
	}

	const periodDays = 30
	g := createMasterGrant(t, ctx, grants, tenantID, masterID,
		wallet.KindFreeSubscriptionPeriod,
		map[string]any{"period_days": periodDays},
		"SIN-63901 idempotency integration test",
		now.Add(-time.Minute),
	)

	if _, err := applier.Apply(ctx, g.ID()); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	applied2, err := applier.Apply(ctx, g.ID())
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if applied2 {
		t.Error("second Apply on consumed grant returned applied=true; want false (no-op)")
	}

	var newEnd time.Time
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT current_period_end FROM subscription WHERE id = $1`, subID,
	).Scan(&newEnd); err != nil {
		t.Fatalf("read period end after rerun: %v", err)
	}
	wantEnd := originalEnd.Add(periodDays * 24 * time.Hour)
	if !newEnd.Equal(wantEnd) {
		t.Errorf("current_period_end after re-apply = %s, want %s (single extension)", newEnd, wantEnd)
	}

	// Subscription must remain active, and NO invoice should have been
	// emitted by either pass.
	var (
		status   string
		invoices int
	)
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT status FROM subscription WHERE id = $1`, subID,
	).Scan(&status); err != nil {
		t.Fatalf("read subscription status: %v", err)
	}
	if status != string(billing.SubscriptionStatusActive) {
		t.Errorf("subscription.status = %q, want active", status)
	}
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM invoice WHERE tenant_id = $1`, tenantID,
	).Scan(&invoices); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	if invoices != 0 {
		t.Errorf("free period rerun created %d invoice(s); want 0", invoices)
	}
}

// ---------- adapter Consume direct tests ----------------------------

// Exercises the postgres MasterGrantStore.Consume path directly,
// covering happy + already-revoked + already-consumed branches that
// the applier integration tests above do not pin down.
func TestMasterGrantStore_Consume_HappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithBillingAndTrigger(t)
	ctx := context.Background()
	tenantID, masterID := seedTenantAndUser(t, ctx, db)

	store, _ := walletadapter.NewMasterGrantStore(db.MasterOpsPool(), masterID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	g, _ := wallet.NewMasterGrant(tenantID, masterID, wallet.KindExtraTokens, map[string]any{"amount": int64(1000)}, "consume happy path integration", now)
	if err := store.Create(ctx, g); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Consume(ctx, g.ID(), "ref-happy", now.Add(time.Second)); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	got, _ := store.GetByID(ctx, g.ID())
	if !got.IsConsumed() {
		t.Fatal("grant should be consumed")
	}
	if got.ConsumedRef() != "ref-happy" {
		t.Errorf("consumed_ref = %q, want ref-happy", got.ConsumedRef())
	}
}

func TestMasterGrantStore_Consume_AlreadyRevoked(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithBillingAndTrigger(t)
	ctx := context.Background()
	tenantID, masterID := seedTenantAndUser(t, ctx, db)

	store, _ := walletadapter.NewMasterGrantStore(db.MasterOpsPool(), masterID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	g, _ := wallet.NewMasterGrant(tenantID, masterID, wallet.KindExtraTokens, map[string]any{"amount": int64(1000)}, "consume after revoke integration", now)
	if err := store.Create(ctx, g); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Revoke(ctx, g.ID(), masterID, "revoke before consume integration", now); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	err := store.Consume(ctx, g.ID(), "ref-no-op", now.Add(time.Second))
	if !errors.Is(err, wallet.ErrGrantAlreadyRevoked) {
		t.Errorf("Consume on revoked: got %v, want ErrGrantAlreadyRevoked", err)
	}
}

func TestMasterGrantStore_Consume_AlreadyConsumed(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithBillingAndTrigger(t)
	ctx := context.Background()
	tenantID, masterID := seedTenantAndUser(t, ctx, db)

	store, _ := walletadapter.NewMasterGrantStore(db.MasterOpsPool(), masterID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	g, _ := wallet.NewMasterGrant(tenantID, masterID, wallet.KindExtraTokens, map[string]any{"amount": int64(1000)}, "consume twice integration test", now)
	if err := store.Create(ctx, g); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Consume(ctx, g.ID(), "ref-first", now); err != nil {
		t.Fatalf("first Consume: %v", err)
	}
	err := store.Consume(ctx, g.ID(), "ref-second", now.Add(time.Second))
	if !errors.Is(err, wallet.ErrGrantAlreadyConsumed) {
		t.Errorf("second Consume: got %v, want ErrGrantAlreadyConsumed", err)
	}
}

func TestMasterGrantStore_Consume_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithBillingAndTrigger(t)
	ctx := context.Background()
	_, masterID := seedTenantAndUser(t, ctx, db)

	store, _ := walletadapter.NewMasterGrantStore(db.MasterOpsPool(), masterID)
	err := store.Consume(ctx, uuid.New(), "ref", time.Now())
	if !errors.Is(err, wallet.ErrNotFound) {
		t.Errorf("Consume on missing: got %v, want ErrNotFound", err)
	}
}
