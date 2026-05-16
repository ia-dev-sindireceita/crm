package postgres_test

// SIN-62879 / Fase 2.5 C5 — adapter tests for the BillingRenewer's two
// ports against the real Postgres testpg harness:
//
//   ListDueSubscriptions returns only active subs whose
//     current_period_end <= asOf, joined with their plan price.
//   RenewSubscription advances the period + inserts a pending invoice
//     atomically; rerunning it for the same period reports
//     billing.ErrInvoiceAlreadyExists.
//
// Lives in the parent postgres_test package (mastersession pattern) to
// avoid the SQLSTATE 28P01 ALTER ROLE race on the shared CI cluster
// (reference_testpg_shared_cluster_race in agent memory).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	billingpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/billing"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/billing"
	billingworker "github.com/pericles-luz/crm/internal/worker/billing"
)

func newRenewerStore(t *testing.T, db *testpg.DB) *billingpg.RenewerStore {
	t.Helper()
	store, err := billingpg.NewRenewerStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("renewer store: %v", err)
	}
	return store
}

// seedRenewerScenario inserts one tenant, one priced plan, and one
// active subscription whose period ended at `endedAt`. Returns the
// tenant/sub/plan ids and the price the test can assert against.
func seedRenewerScenario(
	t *testing.T, ctx context.Context, db *testpg.DB, label string, priceCents int, endedAt time.Time,
) (tenantID, subID, planID uuid.UUID, price int) {
	t.Helper()
	tenantID = seedTenantForBilling(t, ctx, db, label)
	var p uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`INSERT INTO plan (slug, name, price_cents_brl, monthly_token_quota)
		 VALUES ($1, $1, $2, 1000000) RETURNING id`,
		label+"-plan", priceCents,
	).Scan(&p); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	planID = p
	price = priceCents

	periodStart := endedAt.AddDate(0, -1, 0)
	store, err := billingpg.New(db.RuntimePool(), db.MasterOpsPool())
	if err != nil {
		t.Fatalf("billing store: %v", err)
	}
	sub, err := billing.NewSubscription(tenantID, planID, periodStart, endedAt, periodStart)
	if err != nil {
		t.Fatalf("new subscription: %v", err)
	}
	if err := store.SaveSubscription(ctx, sub, uuid.New()); err != nil {
		t.Fatalf("save subscription: %v", err)
	}
	subID = sub.ID()
	return
}

func TestRenewerStore_ListDueSubscriptions_BoundaryAndJoin(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newRenewerStore(t, db)

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	// One due (period ended yesterday), one not yet due (period ends tomorrow).
	dueEnd := now.Add(-24 * time.Hour)
	notDueEnd := now.Add(24 * time.Hour)
	_, dueSubID, _, duePrice := seedRenewerScenario(t, ctx, db, "renew-due", 4990, dueEnd)
	_, notDueSubID, _, _ := seedRenewerScenario(t, ctx, db, "renew-not-due", 9990, notDueEnd)

	got, err := store.ListDueSubscriptions(ctx, now, 100)
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (only one is due)", len(got))
	}
	if got[0].ID != dueSubID {
		t.Errorf("got subID = %v, want %v", got[0].ID, dueSubID)
	}
	if got[0].PlanPriceCents != duePrice {
		t.Errorf("got price = %d, want %d", got[0].PlanPriceCents, duePrice)
	}

	// Sanity: bumping asOf to include the not-due row picks it up too.
	got2, err := store.ListDueSubscriptions(ctx, notDueEnd.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("list due (wide): %v", err)
	}
	var sawNotDue bool
	for _, d := range got2 {
		if d.ID == notDueSubID {
			sawNotDue = true
		}
	}
	if !sawNotDue {
		t.Errorf("expected not-due sub in wider listing, got %v", got2)
	}
}

func TestRenewerStore_ListDueSubscriptions_LimitDefault(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newRenewerStore(t, db)

	// Seed two due subs; ask with limit=0 (should default to 100, so we see both).
	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	seedRenewerScenario(t, ctx, db, "renew-limit-a", 100, now.Add(-time.Hour))
	seedRenewerScenario(t, ctx, db, "renew-limit-b", 200, now.Add(-time.Hour))

	got, err := store.ListDueSubscriptions(ctx, now, 0)
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len(got) = %d, want 2", len(got))
	}
}

func TestRenewerStore_RenewSubscription_AdvancesAndCreatesInvoice(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newRenewerStore(t, db)

	now := time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)
	dueEnd := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	_, subID, _, price := seedRenewerScenario(t, ctx, db, "renew-advance", 4990, dueEnd)

	actor := uuid.New()
	res, err := store.RenewSubscription(ctx, subID, dueEnd, price, actor, now)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}

	wantNewEnd := dueEnd.AddDate(0, 1, 0)
	if !res.NewPeriodStart.Equal(dueEnd) {
		t.Errorf("NewPeriodStart = %v, want %v", res.NewPeriodStart, dueEnd)
	}
	if !res.NewPeriodEnd.Equal(wantNewEnd) {
		t.Errorf("NewPeriodEnd = %v, want %v", res.NewPeriodEnd, wantNewEnd)
	}
	if res.Invoice == nil || res.Invoice.AmountCentsBRL() != price {
		t.Errorf("invoice = %+v, want price %d", res.Invoice, price)
	}
	if res.Invoice.State() != billing.InvoiceStatePending {
		t.Errorf("invoice state = %q, want pending", res.Invoice.State())
	}

	// Verify the subscription advanced in the DB.
	var (
		gotStart, gotEnd time.Time
	)
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), actor, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT current_period_start, current_period_end FROM subscription WHERE id = $1`,
			subID,
		).Scan(&gotStart, &gotEnd)
	}); err != nil {
		t.Fatalf("read advanced sub: %v", err)
	}
	if !gotStart.Equal(dueEnd) {
		t.Errorf("sub.current_period_start = %v, want %v", gotStart, dueEnd)
	}
	if !gotEnd.Equal(wantNewEnd) {
		t.Errorf("sub.current_period_end = %v, want %v", gotEnd, wantNewEnd)
	}

	// Verify the invoice landed with the new period.
	var (
		gotInvStart, gotInvEnd time.Time
		gotAmount              int
		gotState               string
	)
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), actor, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT period_start, period_end, amount_cents_brl, state
			   FROM invoice WHERE id = $1`, res.Invoice.ID(),
		).Scan(&gotInvStart, &gotInvEnd, &gotAmount, &gotState)
	}); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if !gotInvStart.Equal(dueEnd) || !gotInvEnd.Equal(wantNewEnd) {
		t.Errorf("invoice period = [%v, %v], want [%v, %v]", gotInvStart, gotInvEnd, dueEnd, wantNewEnd)
	}
	if gotAmount != price {
		t.Errorf("invoice amount = %d, want %d", gotAmount, price)
	}
	if gotState != string(billing.InvoiceStatePending) {
		t.Errorf("invoice state = %q, want %q", gotState, billing.InvoiceStatePending)
	}
}

func TestRenewerStore_RenewSubscription_IdempotentSameDay(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newRenewerStore(t, db)

	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	dueEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	_, subID, _, price := seedRenewerScenario(t, ctx, db, "renew-idem", 4990, dueEnd)

	actor := uuid.New()
	if _, err := store.RenewSubscription(ctx, subID, dueEnd, price, actor, now); err != nil {
		t.Fatalf("first renew: %v", err)
	}

	// Second call with the SAME oldPeriodEnd must report ErrInvoiceAlreadyExists.
	// The subscription has already advanced, so the optimistic lock on
	// current_period_end fails — but the partial UNIQUE on invoice also
	// catches it. Either path surfaces the same sentinel.
	_, err := store.RenewSubscription(ctx, subID, dueEnd, price, actor, now)
	if !errors.Is(err, billing.ErrInvoiceAlreadyExists) {
		t.Errorf("second renew: got err %v, want ErrInvoiceAlreadyExists", err)
	}

	// Confirm exactly one invoice exists for this subscription.
	var count int
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), actor, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM invoice WHERE subscription_id = $1`, subID,
		).Scan(&count)
	}); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	if count != 1 {
		t.Errorf("invoice count = %d, want 1 (idempotency CA #6)", count)
	}
}

func TestRenewerStore_RenewSubscription_UnknownSubReturnsNotFound(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newRenewerStore(t, db)

	// No subscription seeded with this id; the lock SELECT returns
	// pgx.ErrNoRows → billing.ErrNotFound.
	_, err := store.RenewSubscription(ctx, uuid.New(), time.Now(), 1000, uuid.New(), time.Now())
	if !errors.Is(err, billing.ErrNotFound) {
		t.Errorf("renew unknown sub: got %v, want ErrNotFound", err)
	}
}

func TestRenewerStore_RenewSubscription_RejectsNilSubID(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newBillingCtx(t)
	store := newRenewerStore(t, db)

	_, err := store.RenewSubscription(ctx, uuid.Nil, time.Now(), 1000, uuid.New(), time.Now())
	if err == nil {
		t.Error("renew with uuid.Nil subID: want error, got nil")
	}
}

func TestRenewerStore_NewRejectsNilPool(t *testing.T) {
	if _, err := billingpg.NewRenewerStore(nil); err == nil {
		t.Error("NewRenewerStore(nil): want error, got nil")
	}
}

// Compile-time sanity: ensure the port interfaces are satisfied by the
// adapter (also covered by var _ in renewer.go, but a direct reference
// in tests helps when refactoring breaks the contract.)
var (
	_ billingworker.DueSubscriptionsLister = (*billingpg.RenewerStore)(nil)
	_ billingworker.SubscriptionRenewer    = (*billingpg.RenewerStore)(nil)
)
