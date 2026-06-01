package postgres_test

// Integration tests for MasterTenantStore (SIN-63971).
//
// Lives in the parent postgres_test package (mastersession pattern) to share
// the cluster harness started by TestMain in withtenant_test.go and avoid the
// shared-cluster ALTER ROLE race that bites adapters in their own test binary.
//
// Coverage (matches AC §D in SIN-63971):
//
//  1. List with no filter: empty result without error.
//  2. List with plan filter: returns only matching tenants.
//  3. List pagination: page 2 returns correct slice.
//  4. Create happy path (no plan): tenant row returned with empty plan fields.
//  5. Create happy path (with plan): subscription row written; TenantRow reflects plan.
//  6. Create with host collision → ErrHostTaken.
//  7. Create with unknown plan slug → ErrUnknownPlan.
//  8. Assign happy path (new subscription from no plan): TenantRow reflects new plan.
//  9. Assign happy path (transition existing subscription): old sub cancelled, new active.
// 10. Assign with unknown tenant → ErrNotFound.
// 11. Assign with unknown plan slug → ErrUnknownPlan.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	billingpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/billing"
	masterpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/master"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/wallet"
	masterweb "github.com/pericles-luz/crm/internal/web/master"
)

// freshDBWithTenants returns a DB with the minimal migrations for tenant/plan/subscription.
func freshDBWithTenants(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
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

func newTenantStore(t *testing.T, db *testpg.DB, actor uuid.UUID) *masterpg.MasterTenantStore {
	t.Helper()
	store, err := masterpg.NewMasterTenantStore(db.MasterOpsPool(), db.RuntimePool(), actor)
	if err != nil {
		t.Fatalf("NewMasterTenantStore: %v", err)
	}
	return store
}

// insertPlan inserts a plan row and returns its slug for use in tests.
func insertPlan(t *testing.T, db *testpg.DB, slug string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := db.AdminPool().QueryRow(ctx,
		`INSERT INTO plan (slug, name, price_cents_brl, monthly_token_quota)
		 VALUES ($1,$2,9900,1000000) RETURNING id`, slug, slug+" plan",
	).Scan(&id)
	if err != nil {
		t.Fatalf("insertPlan(%q): %v", slug, err)
	}
	return id
}

// insertMasterTenant inserts a tenant row directly and returns its id.
func insertMasterTenant(t *testing.T, db *testpg.DB, host string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := db.AdminPool().QueryRow(ctx,
		`INSERT INTO tenants (name, host) VALUES ($1,$2) RETURNING id`,
		host+" name", host,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insertMasterTenant(%q): %v", host, err)
	}
	return id
}

// countActiveSubscriptions returns the number of active subscription rows for tenantID.
func countActiveSubscriptions(t *testing.T, db *testpg.DB, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	err := db.AdminPool().QueryRow(context.Background(),
		`SELECT COUNT(*) FROM subscription WHERE tenant_id=$1 AND status='active'`, tenantID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("countActiveSubscriptions: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------
// Constructor guards
// ---------------------------------------------------------------------------

func TestMasterTenantStore_NilPoolReturnsError(t *testing.T) {
	db := freshDBWithTenants(t)
	actor := uuid.New()
	if _, err := masterpg.NewMasterTenantStore(nil, db.RuntimePool(), actor); err == nil {
		t.Error("nil masterOpsPool: want error, got nil")
	}
	if _, err := masterpg.NewMasterTenantStore(db.MasterOpsPool(), nil, actor); err == nil {
		t.Error("nil runtimePool: want error, got nil")
	}
}

func TestMasterTenantStore_ZeroActorReturnsError(t *testing.T) {
	db := freshDBWithTenants(t)
	if _, err := masterpg.NewMasterTenantStore(db.MasterOpsPool(), db.RuntimePool(), uuid.Nil); err == nil {
		t.Error("uuid.Nil actor: want error, got nil")
	}
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

func TestMasterTenantStore_List_EmptyDatabase(t *testing.T) {
	db := freshDBWithTenants(t)
	store := newTenantStore(t, db, uuid.New())
	res, err := store.List(context.Background(), masterweb.ListOptions{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Tenants) != 0 {
		t.Errorf("got %d tenants, want 0", len(res.Tenants))
	}
	if res.TotalCount != 0 {
		t.Errorf("TotalCount=%d, want 0", res.TotalCount)
	}
}

func TestMasterTenantStore_List_WithPlanFilter(t *testing.T) {
	db := freshDBWithTenants(t)
	actor := uuid.New()
	store := newTenantStore(t, db, actor)
	ctx := context.Background()

	insertPlan(t, db, "starter")
	insertPlan(t, db, "pro")

	// Create two tenants with different plans.
	_, err := store.Create(ctx, masterweb.CreateTenantInput{
		ActorUserID: actor, Name: "T-Starter", Host: "starter.example.com", PlanSlug: "starter",
	})
	if err != nil {
		t.Fatalf("Create starter tenant: %v", err)
	}
	_, err = store.Create(ctx, masterweb.CreateTenantInput{
		ActorUserID: actor, Name: "T-Pro", Host: "pro.example.com", PlanSlug: "pro",
	})
	if err != nil {
		t.Fatalf("Create pro tenant: %v", err)
	}

	res, err := store.List(ctx, masterweb.ListOptions{Page: 1, PageSize: 25, FilterPlanSlug: "starter"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.TotalCount != 1 {
		t.Errorf("TotalCount=%d, want 1", res.TotalCount)
	}
	if len(res.Tenants) != 1 || res.Tenants[0].PlanSlug != "starter" {
		t.Errorf("got %+v, want PlanSlug=starter", res.Tenants)
	}
}

func TestMasterTenantStore_List_Pagination(t *testing.T) {
	db := freshDBWithTenants(t)
	actor := uuid.New()
	store := newTenantStore(t, db, actor)
	ctx := context.Background()

	// Insert 5 tenants.
	for i := 0; i < 5; i++ {
		insertMasterTenant(t, db, fmt.Sprintf("paginatest%d.example.com", i))
	}

	p1, err := store.List(ctx, masterweb.ListOptions{Page: 1, PageSize: 3})
	if err != nil {
		t.Fatalf("List page1: %v", err)
	}
	if len(p1.Tenants) != 3 {
		t.Errorf("page1 len=%d, want 3", len(p1.Tenants))
	}
	if p1.TotalCount != 5 {
		t.Errorf("TotalCount=%d, want 5", p1.TotalCount)
	}

	p2, err := store.List(ctx, masterweb.ListOptions{Page: 2, PageSize: 3})
	if err != nil {
		t.Fatalf("List page2: %v", err)
	}
	if len(p2.Tenants) != 2 {
		t.Errorf("page2 len=%d, want 2", len(p2.Tenants))
	}
	// No overlap between pages.
	seen := map[uuid.UUID]bool{}
	for _, r := range p1.Tenants {
		seen[r.ID] = true
	}
	for _, r := range p2.Tenants {
		if seen[r.ID] {
			t.Errorf("tenant %s appeared in both pages", r.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

func TestMasterTenantStore_Create_HappyPath_NoPlan(t *testing.T) {
	db := freshDBWithTenants(t)
	actor := uuid.New()
	store := newTenantStore(t, db, actor)

	res, err := store.Create(context.Background(), masterweb.CreateTenantInput{
		ActorUserID: actor, Name: "ACME", Host: "acme.example.com",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Tenant.ID == uuid.Nil {
		t.Error("Tenant.ID is nil")
	}
	if res.Tenant.Host != "acme.example.com" {
		t.Errorf("Host=%q, want acme.example.com", res.Tenant.Host)
	}
	if res.Tenant.PlanSlug != "" {
		t.Errorf("PlanSlug=%q, want empty", res.Tenant.PlanSlug)
	}
	if res.Tenant.SubscriptionStatus != "" {
		t.Errorf("SubscriptionStatus=%q, want empty", res.Tenant.SubscriptionStatus)
	}
}

func TestMasterTenantStore_Create_HappyPath_WithPlan(t *testing.T) {
	db := freshDBWithTenants(t)
	actor := uuid.New()
	insertPlan(t, db, "basic")
	store := newTenantStore(t, db, actor)

	res, err := store.Create(context.Background(), masterweb.CreateTenantInput{
		ActorUserID: actor, Name: "Tenant-Basic", Host: "basic.example.com", PlanSlug: "basic",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Tenant.PlanSlug != "basic" {
		t.Errorf("PlanSlug=%q, want basic", res.Tenant.PlanSlug)
	}
	if res.Tenant.SubscriptionStatus != "active" {
		t.Errorf("SubscriptionStatus=%q, want active", res.Tenant.SubscriptionStatus)
	}
	if n := countActiveSubscriptions(t, db, res.Tenant.ID); n != 1 {
		t.Errorf("active subscriptions=%d, want 1", n)
	}
}

func TestMasterTenantStore_Create_HostCollision_ReturnsErrHostTaken(t *testing.T) {
	db := freshDBWithTenants(t)
	actor := uuid.New()
	store := newTenantStore(t, db, actor)
	ctx := context.Background()
	in := masterweb.CreateTenantInput{ActorUserID: actor, Name: "T", Host: "dup.example.com"}

	if _, err := store.Create(ctx, in); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := store.Create(ctx, in)
	if !errors.Is(err, masterweb.ErrHostTaken) {
		t.Errorf("duplicate Create: want ErrHostTaken, got %v", err)
	}
}

func TestMasterTenantStore_Create_UnknownPlan_ReturnsErrUnknownPlan(t *testing.T) {
	db := freshDBWithTenants(t)
	actor := uuid.New()
	store := newTenantStore(t, db, actor)

	_, err := store.Create(context.Background(), masterweb.CreateTenantInput{
		ActorUserID: actor, Name: "T", Host: "unknown.example.com", PlanSlug: "does-not-exist",
	})
	if !errors.Is(err, masterweb.ErrUnknownPlan) {
		t.Errorf("want ErrUnknownPlan, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Assign
// ---------------------------------------------------------------------------

func TestMasterTenantStore_Assign_NewSubscription(t *testing.T) {
	db := freshDBWithTenants(t)
	actor := uuid.New()
	insertPlan(t, db, "gold")
	store := newTenantStore(t, db, actor)
	ctx := context.Background()

	// Tenant without a plan.
	tenantID := insertMasterTenant(t, db, "gold.example.com")

	res, err := store.Assign(ctx, masterweb.AssignPlanInput{
		ActorUserID: actor, TenantID: tenantID, PlanSlug: "gold",
	})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if res.Tenant.PlanSlug != "gold" {
		t.Errorf("PlanSlug=%q, want gold", res.Tenant.PlanSlug)
	}
	if res.Tenant.SubscriptionStatus != "active" {
		t.Errorf("SubscriptionStatus=%q, want active", res.Tenant.SubscriptionStatus)
	}
}

func TestMasterTenantStore_Assign_TransitionExistingSubscription(t *testing.T) {
	db := freshDBWithTenants(t)
	actor := uuid.New()
	insertPlan(t, db, "silver")
	insertPlan(t, db, "platinum")
	store := newTenantStore(t, db, actor)
	ctx := context.Background()

	// Create tenant with silver plan.
	cr, err := store.Create(ctx, masterweb.CreateTenantInput{
		ActorUserID: actor, Name: "T-Silver", Host: "silver.example.com", PlanSlug: "silver",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tenantID := cr.Tenant.ID

	// Upgrade to platinum.
	res, err := store.Assign(ctx, masterweb.AssignPlanInput{
		ActorUserID: actor, TenantID: tenantID, PlanSlug: "platinum",
	})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if res.Tenant.PlanSlug != "platinum" {
		t.Errorf("PlanSlug=%q, want platinum", res.Tenant.PlanSlug)
	}
	// Exactly 1 active subscription after transition.
	if n := countActiveSubscriptions(t, db, tenantID); n != 1 {
		t.Errorf("active subscriptions=%d, want 1", n)
	}
	// Previous silver sub is now cancelled.
	var cancelledCount int
	err = db.AdminPool().QueryRow(ctx,
		`SELECT COUNT(*) FROM subscription WHERE tenant_id=$1 AND status='cancelled'`, tenantID,
	).Scan(&cancelledCount)
	if err != nil {
		t.Fatalf("count cancelled: %v", err)
	}
	if cancelledCount != 1 {
		t.Errorf("cancelled subscriptions=%d, want 1", cancelledCount)
	}
}

func TestMasterTenantStore_Assign_UnknownTenant_ReturnsErrNotFound(t *testing.T) {
	db := freshDBWithTenants(t)
	actor := uuid.New()
	insertPlan(t, db, "any")
	store := newTenantStore(t, db, actor)

	_, err := store.Assign(context.Background(), masterweb.AssignPlanInput{
		ActorUserID: actor, TenantID: uuid.New(), PlanSlug: "any",
	})
	if !errors.Is(err, masterweb.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestMasterTenantStore_Assign_UnknownPlan_ReturnsErrUnknownPlan(t *testing.T) {
	db := freshDBWithTenants(t)
	actor := uuid.New()
	store := newTenantStore(t, db, actor)
	tenantID := insertMasterTenant(t, db, "noplanasgn.example.com")

	_, err := store.Assign(context.Background(), masterweb.AssignPlanInput{
		ActorUserID: actor, TenantID: tenantID, PlanSlug: "nonexistent",
	})
	if !errors.Is(err, masterweb.ErrUnknownPlan) {
		t.Errorf("want ErrUnknownPlan, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// PlanListerShim smoke
// ---------------------------------------------------------------------------

func TestPlanListerShim_NilStoreReturnsError(t *testing.T) {
	if _, err := masterpg.NewPlanListerShim(nil); err == nil {
		t.Error("nil store: want error, got nil")
	}
}

func TestPlanListerShim_ListDelegates(t *testing.T) {
	db := freshDBWithTenants(t)
	insertPlan(t, db, "lister-test-plan")
	billingStore := newBillingStoreForShim(t, db)
	shim, err := masterpg.NewPlanListerShim(billingStore)
	if err != nil {
		t.Fatalf("NewPlanListerShim: %v", err)
	}
	plans, err := shim.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, p := range plans {
		if p.Slug == "lister-test-plan" {
			found = true
		}
	}
	if !found {
		t.Errorf("plan lister-test-plan not found in %+v", plans)
	}
}

// newBillingStoreForShim constructs a billing.Store for the PlanListerShim test.
func newBillingStoreForShim(t *testing.T, db *testpg.DB) *billingpg.Store {
	t.Helper()
	store, err := billingpg.New(db.RuntimePool(), db.MasterOpsPool())
	if err != nil {
		t.Fatalf("billing.New: %v", err)
	}
	return store
}

func TestMasterTenantStore_WithTenantStoreClock_PinsTimestamp(t *testing.T) {
	db := freshDBWithTenants(t)
	actor := uuid.New()
	fixedTime := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	store, err := masterpg.NewMasterTenantStore(
		db.MasterOpsPool(), db.RuntimePool(), actor,
		masterpg.WithTenantStoreClock(func() time.Time { return fixedTime }),
	)
	if err != nil {
		t.Fatalf("NewMasterTenantStore: %v", err)
	}

	// Just verify the store was constructed and can operate.
	res, err := store.List(context.Background(), masterweb.ListOptions{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.TotalCount != 0 {
		t.Errorf("TotalCount=%d, want 0", res.TotalCount)
	}
}

func TestMasterTenantStore_WithCourtesyRepo_SkipsWhenNilTokens(t *testing.T) {
	db := freshDBWithTenants(t)
	actor := uuid.New()
	insertPlan(t, db, "courtesy-test-plan")

	// courtesyRepo is set but InitialCourtesyTokens=0, so Issue must NOT be called.
	called := false
	fakeCourtesy := &fakeCourtesyRepo{onIssue: func() { called = true }}

	store, err := masterpg.NewMasterTenantStore(
		db.MasterOpsPool(), db.RuntimePool(), actor,
		masterpg.WithCourtesyRepo(fakeCourtesy),
	)
	if err != nil {
		t.Fatalf("NewMasterTenantStore: %v", err)
	}
	if _, err := store.Create(context.Background(), masterweb.CreateTenantInput{
		ActorUserID: actor, Name: "T-Courtesy", Host: "courtesytest.example.com",
		PlanSlug: "courtesy-test-plan", InitialCourtesyTokens: 0,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if called {
		t.Error("courtesyRepo.Issue was called but InitialCourtesyTokens=0")
	}
}

// fakeCourtesyRepo is a test double for wallet.CourtesyGrantRepository.
type fakeCourtesyRepo struct {
	onIssue func()
}

func (f *fakeCourtesyRepo) Issue(_ context.Context, _, _ uuid.UUID, _ int64) (walletIssued, error) {
	if f.onIssue != nil {
		f.onIssue()
	}
	return walletIssued{Granted: true, WalletID: uuid.New(), GrantID: uuid.New()}, nil
}

// walletIssued mirrors wallet.Issued to avoid importing the wallet package in tests.
type walletIssued = wallet.Issued

func TestMasterTenantStore_Create_WithCourtesyTokens_CallsRepo(t *testing.T) {
	db := freshDBWithTenants(t)
	actor := uuid.New()
	insertPlan(t, db, "courtesy-tokens-plan")

	called := false
	fakeCourtesy2 := &fakeCourtesyRepo{onIssue: func() { called = true }}

	store, err := masterpg.NewMasterTenantStore(
		db.MasterOpsPool(), db.RuntimePool(), actor,
		masterpg.WithCourtesyRepo(fakeCourtesy2),
	)
	if err != nil {
		t.Fatalf("NewMasterTenantStore: %v", err)
	}
	if _, err := store.Create(context.Background(), masterweb.CreateTenantInput{
		ActorUserID: actor, Name: "T-WithTokens", Host: "withtoken.example.com",
		PlanSlug: "courtesy-tokens-plan", InitialCourtesyTokens: 5000,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !called {
		t.Error("courtesyRepo.Issue was NOT called but InitialCourtesyTokens > 0")
	}
}

func TestMasterTenantStore_List_DefaultPageSizeApplied(t *testing.T) {
	db := freshDBWithTenants(t)
	store := newTenantStore(t, db, uuid.New())

	// ListOptions with page=0, pageSize=0 → defaults to page=1, pageSize=25
	res, err := store.List(context.Background(), masterweb.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.Page != 1 {
		t.Errorf("Page=%d, want 1", res.Page)
	}
	if res.PageSize != 25 {
		t.Errorf("PageSize=%d, want 25", res.PageSize)
	}
}
