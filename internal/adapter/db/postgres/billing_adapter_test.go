package postgres_test

// SIN-62877 adapter tests for billing.PlanCatalog, billing.SubscriptionRepository,
// and billing.InvoiceRepository.
//
// freshDBWithBilling, seedPlan (5-arg), and seedTenantUserMaster are defined
// in subscription_billing_migration_test.go and audit_helpers_test.go
// respectively — they share this package.
//
// Tests live in the parent postgres_test package (not the billing sub-package)
// to share the single TestMain / testpg.Harness and avoid the ALTER ROLE race
// on the shared CI cluster (reference_testpg_shared_cluster_race).

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	billingpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/billing"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/billing"
)

func newBillingCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newBillingStore(t *testing.T, db *testpg.DB) *billingpg.Store {
	t.Helper()
	store, err := billingpg.New(db.RuntimePool(), db.MasterOpsPool())
	if err != nil {
		t.Fatalf("billing store: %v", err)
	}
	return store
}

var (
	billingPeriodStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	billingPeriodEnd   = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	billingNow         = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
)

// seedTenantForBilling inserts a tenant row using the admin pool.
func seedTenantForBilling(t *testing.T, ctx context.Context, db *testpg.DB, label string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, label, fmt.Sprintf("%s-%s.crm.local", label, id),
	); err != nil {
		t.Fatalf("seed tenant %s: %v", label, err)
	}
	return id
}

// seedSubscriptionForBilling creates and persists an active Subscription.
func seedSubscriptionForBilling(t *testing.T, ctx context.Context, store *billingpg.Store, tenantID, planID uuid.UUID) *billing.Subscription {
	t.Helper()
	sub, err := billing.NewSubscription(tenantID, planID, billingPeriodStart, billingPeriodEnd, billingNow)
	if err != nil {
		t.Fatalf("new subscription: %v", err)
	}
	if err := store.SaveSubscription(ctx, sub, uuid.New()); err != nil {
		t.Fatalf("save subscription: %v", err)
	}
	return sub
}

// ---------------------------------------------------------------------------
// PlanCatalog tests
// ---------------------------------------------------------------------------

func TestBilling_ListPlans_Empty(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	plans, err := store.ListPlans(ctx)
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	if len(plans) != 0 {
		t.Errorf("expected 0 plans, got %d", len(plans))
	}
}

func TestBilling_PlanCRUD(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	// Seed two plans with explicit prices.
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO plan (slug, name, price_cents_brl, monthly_token_quota)
		 VALUES ('free-billing',0,0,10000),('pro-billing','Pro',4990,1000000)`); err != nil {
		t.Fatalf("seed plans: %v", err)
	}

	t.Run("list ordered by price", func(t *testing.T) {
		plans, err := store.ListPlans(ctx)
		if err != nil {
			t.Fatalf("list plans: %v", err)
		}
		if len(plans) != 2 {
			t.Fatalf("expected 2, got %d", len(plans))
		}
		if plans[0].PriceCentsBRL > plans[1].PriceCentsBRL {
			t.Errorf("list not ordered ASC: %d > %d", plans[0].PriceCentsBRL, plans[1].PriceCentsBRL)
		}
	})

	t.Run("get by slug", func(t *testing.T) {
		p, err := store.GetBySlug(ctx, "pro-billing")
		if err != nil {
			t.Fatalf("get by slug: %v", err)
		}
		if p.PriceCentsBRL != 4990 {
			t.Errorf("price mismatch: %d", p.PriceCentsBRL)
		}
		if p.MonthlyTokenQuota != 1000000 {
			t.Errorf("quota mismatch: %d", p.MonthlyTokenQuota)
		}
	})

	t.Run("get by slug not found", func(t *testing.T) {
		_, err := store.GetBySlug(ctx, "does-not-exist")
		if !errors.Is(err, billing.ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("get by id roundtrips quota", func(t *testing.T) {
		seed, err := store.GetBySlug(ctx, "pro-billing")
		if err != nil {
			t.Fatalf("get by slug to capture id: %v", err)
		}
		p, err := store.GetPlanByID(ctx, seed.ID)
		if err != nil {
			t.Fatalf("get by id: %v", err)
		}
		if p.Slug != "pro-billing" {
			t.Errorf("slug = %q, want pro-billing", p.Slug)
		}
		if p.MonthlyTokenQuota != 1000000 {
			t.Errorf("quota = %d, want 1000000", p.MonthlyTokenQuota)
		}
	})

	t.Run("get by id not found", func(t *testing.T) {
		_, err := store.GetPlanByID(ctx, uuid.New())
		if !errors.Is(err, billing.ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("get by id zero uuid", func(t *testing.T) {
		_, err := store.GetPlanByID(ctx, uuid.Nil)
		if !errors.Is(err, billing.ErrNotFound) {
			t.Errorf("expected ErrNotFound for zero id, got %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// SubscriptionRepository tests
// ---------------------------------------------------------------------------

func TestBilling_SubscriptionNotFound(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	tenantID := seedTenantForBilling(t, ctx, db, "sub-notfound")
	_, err := store.GetByTenant(ctx, tenantID)
	if !errors.Is(err, billing.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestBilling_SubscriptionZeroTenant(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	_, err := store.GetByTenant(ctx, uuid.Nil)
	if !errors.Is(err, billing.ErrZeroTenant) {
		t.Errorf("expected ErrZeroTenant, got %v", err)
	}
}

func TestBilling_SubscriptionSaveAndGet(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	tenantID := seedTenantForBilling(t, ctx, db, "sub-save")
	planID := seedPlan(t, ctx, db, "sub-save-plan", 500_000)
	actorID := uuid.New()

	sub, err := billing.NewSubscription(tenantID, planID, billingPeriodStart, billingPeriodEnd, billingNow)
	if err != nil {
		t.Fatalf("new subscription: %v", err)
	}
	if err := store.SaveSubscription(ctx, sub, actorID); err != nil {
		t.Fatalf("save subscription: %v", err)
	}

	got, err := store.GetByTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	if got.ID() != sub.ID() {
		t.Error("id mismatch")
	}
	if got.Status() != billing.SubscriptionStatusActive {
		t.Errorf("expected active, got %s", got.Status())
	}
	if got.PlanID() != planID {
		t.Error("plan id mismatch")
	}
}

func TestBilling_SubscriptionCancel(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	tenantID := seedTenantForBilling(t, ctx, db, "sub-cancel")
	planID := seedPlan(t, ctx, db, "sub-cancel-plan", 0)
	actorID := uuid.New()

	sub, _ := billing.NewSubscription(tenantID, planID, billingPeriodStart, billingPeriodEnd, billingNow)
	if err := store.SaveSubscription(ctx, sub, actorID); err != nil {
		t.Fatalf("initial save: %v", err)
	}

	if err := sub.Cancel(billingNow); err != nil {
		t.Fatalf("cancel domain: %v", err)
	}
	if err := store.SaveSubscription(ctx, sub, actorID); err != nil {
		t.Fatalf("save cancelled: %v", err)
	}

	_, err := store.GetByTenant(ctx, tenantID)
	if !errors.Is(err, billing.ErrNotFound) {
		t.Errorf("expected ErrNotFound after cancel, got %v", err)
	}
}

func TestBilling_SubscriptionRLS(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	tenantA := seedTenantForBilling(t, ctx, db, "sub-rls-a")
	tenantB := seedTenantForBilling(t, ctx, db, "sub-rls-b")
	planID := seedPlan(t, ctx, db, "sub-rls-plan", 0)

	_ = seedSubscriptionForBilling(t, ctx, store, tenantA, planID)

	// Tenant B must not see tenant A's subscription.
	_, err := store.GetByTenant(ctx, tenantB)
	if !errors.Is(err, billing.ErrNotFound) {
		t.Errorf("RLS breach: tenant B got tenant A's subscription, err=%v", err)
	}
}

func TestBilling_SaveSubscription_DuplicateActive(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	tenantID := seedTenantForBilling(t, ctx, db, "sub-dup")
	planID := seedPlan(t, ctx, db, "sub-dup-plan", 0)
	actorID := uuid.New()

	sub1, _ := billing.NewSubscription(tenantID, planID, billingPeriodStart, billingPeriodEnd, billingNow)
	if err := store.SaveSubscription(ctx, sub1, actorID); err != nil {
		t.Fatalf("save first subscription: %v", err)
	}

	// A second ACTIVE subscription for the same tenant (different ID) must
	// trip the partial UNIQUE index and surface as ErrInvalidTransition.
	sub2, _ := billing.NewSubscription(tenantID, planID,
		time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		billingNow)
	err := store.SaveSubscription(ctx, sub2, actorID)
	if !errors.Is(err, billing.ErrInvalidTransition) {
		t.Errorf("expected ErrInvalidTransition for duplicate active subscription, got %v", err)
	}
}

func TestBilling_SaveSubscription_NilRejected(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	if err := store.SaveSubscription(ctx, nil, uuid.New()); err == nil {
		t.Error("expected error for nil subscription")
	}
}

// ---------------------------------------------------------------------------
// InvoiceRepository tests
// ---------------------------------------------------------------------------

func TestBilling_InvoiceSaveAndGet(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	tenantID := seedTenantForBilling(t, ctx, db, "inv-save")
	planID := seedPlan(t, ctx, db, "inv-save-plan", 1_000_000)
	sub := seedSubscriptionForBilling(t, ctx, store, tenantID, planID)
	actorID := uuid.New()

	inv, err := billing.NewInvoice(tenantID, sub.ID(), billingPeriodStart, billingPeriodEnd, 4990, billingNow)
	if err != nil {
		t.Fatalf("new invoice: %v", err)
	}
	if err := store.SaveInvoice(ctx, inv, actorID); err != nil {
		t.Fatalf("save invoice: %v", err)
	}

	got, err := store.GetByID(ctx, tenantID, inv.ID())
	if err != nil {
		t.Fatalf("get invoice: %v", err)
	}
	if got.ID() != inv.ID() {
		t.Error("id mismatch")
	}
	if got.State() != billing.InvoiceStatePending {
		t.Errorf("expected pending, got %s", got.State())
	}
	if got.AmountCentsBRL() != 4990 {
		t.Errorf("amount mismatch: %d", got.AmountCentsBRL())
	}
}

func TestBilling_InvoiceMarkPaid_Idempotent(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	tenantID := seedTenantForBilling(t, ctx, db, "inv-paid")
	planID := seedPlan(t, ctx, db, "inv-paid-plan", 1_000_000)
	sub := seedSubscriptionForBilling(t, ctx, store, tenantID, planID)
	actorID := uuid.New()

	inv, _ := billing.NewInvoice(tenantID, sub.ID(), billingPeriodStart, billingPeriodEnd, 4990, billingNow)
	if err := store.SaveInvoice(ctx, inv, actorID); err != nil {
		t.Fatalf("save invoice: %v", err)
	}

	if err := inv.MarkPaid(billingNow); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	// Save twice — second save is upsert-by-id, must not error.
	if err := store.SaveInvoice(ctx, inv, actorID); err != nil {
		t.Fatalf("save paid (1): %v", err)
	}
	if err := store.SaveInvoice(ctx, inv, actorID); err != nil {
		t.Fatalf("save paid (2 - idempotent): %v", err)
	}

	got, _ := store.GetByID(ctx, tenantID, inv.ID())
	if got.State() != billing.InvoiceStatePaid {
		t.Errorf("expected paid, got %s", got.State())
	}
}

func TestBilling_InvoiceAlreadyExists(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	tenantID := seedTenantForBilling(t, ctx, db, "inv-dup")
	planID := seedPlan(t, ctx, db, "inv-dup-plan", 1_000_000)
	sub := seedSubscriptionForBilling(t, ctx, store, tenantID, planID)
	actorID := uuid.New()

	inv1, _ := billing.NewInvoice(tenantID, sub.ID(), billingPeriodStart, billingPeriodEnd, 4990, billingNow)
	if err := store.SaveInvoice(ctx, inv1, actorID); err != nil {
		t.Fatalf("save inv1: %v", err)
	}

	// Different ID, same (tenant_id, period_start) and non-cancelled → unique violation.
	inv2, _ := billing.NewInvoice(tenantID, sub.ID(), billingPeriodStart, billingPeriodEnd, 4990, billingNow)
	err := store.SaveInvoice(ctx, inv2, actorID)
	if !errors.Is(err, billing.ErrInvoiceAlreadyExists) {
		t.Errorf("expected ErrInvoiceAlreadyExists, got %v", err)
	}
}

func TestBilling_InvoiceAlreadyExists_AfterCancel_AllowsNew(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	tenantID := seedTenantForBilling(t, ctx, db, "inv-reissue")
	planID := seedPlan(t, ctx, db, "inv-reissue-plan", 1_000_000)
	sub := seedSubscriptionForBilling(t, ctx, store, tenantID, planID)
	actorID := uuid.New()

	inv1, _ := billing.NewInvoice(tenantID, sub.ID(), billingPeriodStart, billingPeriodEnd, 4990, billingNow)
	if err := store.SaveInvoice(ctx, inv1, actorID); err != nil {
		t.Fatalf("save inv1: %v", err)
	}

	if err := inv1.CancelByMaster("testing cancellation for re-issue", billingNow); err != nil {
		t.Fatalf("cancel domain: %v", err)
	}
	if err := store.SaveInvoice(ctx, inv1, actorID); err != nil {
		t.Fatalf("save cancelled inv1: %v", err)
	}

	// Fresh invoice for the same period should be accepted now.
	inv2, _ := billing.NewInvoice(tenantID, sub.ID(), billingPeriodStart, billingPeriodEnd, 4990, billingNow)
	if err := store.SaveInvoice(ctx, inv2, actorID); err != nil {
		t.Errorf("re-issue after cancel should succeed, got %v", err)
	}
}

func TestBilling_InvoiceCancelByMaster(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	tenantID := seedTenantForBilling(t, ctx, db, "inv-cancel")
	planID := seedPlan(t, ctx, db, "inv-cancel-plan", 0)
	sub := seedSubscriptionForBilling(t, ctx, store, tenantID, planID)
	actorID := uuid.New()

	inv, _ := billing.NewInvoice(tenantID, sub.ID(), billingPeriodStart, billingPeriodEnd, 0, billingNow)
	if err := store.SaveInvoice(ctx, inv, actorID); err != nil {
		t.Fatalf("save invoice: %v", err)
	}

	const reason = "cancelled for testing purposes"
	if err := inv.CancelByMaster(reason, billingNow); err != nil {
		t.Fatalf("cancel domain: %v", err)
	}
	if err := store.SaveInvoice(ctx, inv, actorID); err != nil {
		t.Fatalf("save cancelled: %v", err)
	}

	got, _ := store.GetByID(ctx, tenantID, inv.ID())
	if got.State() != billing.InvoiceStateCancelledByMaster {
		t.Errorf("expected cancelled_by_master, got %s", got.State())
	}
	if got.CancelledReason() != reason {
		t.Errorf("reason mismatch: %q", got.CancelledReason())
	}
}

func TestBilling_InvoiceListByTenant(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	tenantID := seedTenantForBilling(t, ctx, db, "inv-list")
	planID := seedPlan(t, ctx, db, "inv-list-plan", 1_000_000)
	sub := seedSubscriptionForBilling(t, ctx, store, tenantID, planID)
	actorID := uuid.New()

	periods := []struct{ start, end time.Time }{
		{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, p := range periods {
		inv, _ := billing.NewInvoice(tenantID, sub.ID(), p.start, p.end, 4990, billingNow)
		if err := store.SaveInvoice(ctx, inv, actorID); err != nil {
			t.Fatalf("save invoice: %v", err)
		}
	}

	list, err := store.ListByTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3, got %d", len(list))
	}
	if !list[0].PeriodStart().After(list[1].PeriodStart()) {
		t.Errorf("list not ordered DESC: %v %v", list[0].PeriodStart(), list[1].PeriodStart())
	}
}

func TestBilling_InvoiceListByTenant_ZeroTenant(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	_, err := store.ListByTenant(ctx, uuid.Nil)
	if !errors.Is(err, billing.ErrZeroTenant) {
		t.Errorf("expected ErrZeroTenant, got %v", err)
	}
}

func TestBilling_InvoiceGetByID_ZeroTenant(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	_, err := store.GetByID(ctx, uuid.Nil, uuid.New())
	if !errors.Is(err, billing.ErrZeroTenant) {
		t.Errorf("expected ErrZeroTenant, got %v", err)
	}
}

func TestBilling_InvoiceRLS(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	tenantA := seedTenantForBilling(t, ctx, db, "inv-rls-a")
	tenantB := seedTenantForBilling(t, ctx, db, "inv-rls-b")
	planID := seedPlan(t, ctx, db, "inv-rls-plan", 0)
	subA := seedSubscriptionForBilling(t, ctx, store, tenantA, planID)
	actorID := uuid.New()

	inv, _ := billing.NewInvoice(tenantA, subA.ID(), billingPeriodStart, billingPeriodEnd, 0, billingNow)
	if err := store.SaveInvoice(ctx, inv, actorID); err != nil {
		t.Fatalf("save invoice: %v", err)
	}

	_, err := store.GetByID(ctx, tenantB, inv.ID())
	if !errors.Is(err, billing.ErrNotFound) {
		t.Errorf("RLS breach: tenant B got tenant A's invoice, err=%v", err)
	}

	list, _ := store.ListByTenant(ctx, tenantB)
	if len(list) != 0 {
		t.Errorf("RLS breach: tenant B sees %d invoices", len(list))
	}
}

func TestBilling_SaveInvoice_NilRejected(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newBillingStore(t, db)

	if err := store.SaveInvoice(ctx, nil, uuid.New()); err == nil {
		t.Error("expected error for nil invoice")
	}
}

func TestBilling_Store_NilPool(t *testing.T) {
	db := freshDBWithBilling(t)

	if _, err := billingpg.New(nil, db.MasterOpsPool()); err == nil {
		t.Error("expected error for nil runtime pool")
	}
	if _, err := billingpg.New(db.RuntimePool(), nil); err == nil {
		t.Error("expected error for nil master pool")
	}
}
