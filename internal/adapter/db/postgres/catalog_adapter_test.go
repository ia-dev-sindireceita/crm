package postgres_test

// SIN-62902 adapter tests for catalog.ProductRepository and
// catalog.ArgumentRepository.
//
// Tests live in the parent postgres_test package (not the catalog
// sub-package) per `lint-postgres-adapter-tests` and the
// reference_testpg_shared_cluster_race rationale (SIN-62750): one
// TestMain per package keeps the shared CI cluster's ALTER ROLE
// bootstrap race a non-event.
//
// The chain freshDBWithCatalog applies is the same one
// freshDBWithAIW1A uses (0004 → 0005 → 0088 → 0098); we keep a
// dedicated helper so a future Fase 3 wave can renumber 0098 without
// breaking the W1A tests' helper out from under them.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	catalogpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/catalog"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/catalog"
)

func freshDBWithCatalog(t *testing.T) (*testpg.DB, context.Context) {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0098_ai_policy_ai_summary_product_argument.up.sql",
	)
	return db, ctx
}

func newCatalogStore(t *testing.T, db *testpg.DB) *catalogpg.Store {
	t.Helper()
	store, err := catalogpg.New(db.RuntimePool(), db.MasterOpsPool())
	if err != nil {
		t.Fatalf("catalog store: %v", err)
	}
	return store
}

var catalogFixedNow = time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

func seedTenantForCatalog(t *testing.T, ctx context.Context, db *testpg.DB, label string) uuid.UUID {
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

// ---------------------------------------------------------------------------
// New
// ---------------------------------------------------------------------------

func TestCatalog_New_RejectsNilPools(t *testing.T) {
	if _, err := catalogpg.New(nil, nil); !errors.Is(err, postgresadapter.ErrNilPool) {
		t.Errorf("New(nil,nil) = %v, want ErrNilPool", err)
	}
}

// ---------------------------------------------------------------------------
// ProductRepository
// ---------------------------------------------------------------------------

func TestCatalog_SaveAndGetProduct(t *testing.T) {
	db, ctx := freshDBWithCatalog(t)
	tenantID := seedTenantForCatalog(t, ctx, db, "save")
	store := newCatalogStore(t, db)
	actorID := uuid.New()

	p, err := catalog.NewProduct(tenantID, "Plan Pro", "the pro plan", 4990,
		[]string{"saas", "monthly"}, catalogFixedNow)
	if err != nil {
		t.Fatalf("NewProduct: %v", err)
	}
	if err := store.SaveProduct(ctx, p, actorID); err != nil {
		t.Fatalf("SaveProduct: %v", err)
	}

	got, err := store.GetByID(ctx, tenantID, p.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name() != "Plan Pro" || got.PriceCents() != 4990 || got.Description() != "the pro plan" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if tags := got.Tags(); len(tags) != 2 || tags[0] != "saas" || tags[1] != "monthly" {
		t.Errorf("tags round-trip: %v", tags)
	}
}

func TestCatalog_GetByID_NotFound(t *testing.T) {
	db, ctx := freshDBWithCatalog(t)
	tenantID := seedTenantForCatalog(t, ctx, db, "404")
	store := newCatalogStore(t, db)

	_, err := store.GetByID(ctx, tenantID, uuid.New())
	if !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("GetByID(missing) = %v, want ErrNotFound", err)
	}
}

func TestCatalog_GetByID_ZeroTenant(t *testing.T) {
	db, _ := freshDBWithCatalog(t)
	store := newCatalogStore(t, db)
	_, err := store.GetByID(context.Background(), uuid.Nil, uuid.New())
	if !errors.Is(err, catalog.ErrZeroTenant) {
		t.Errorf("GetByID(uuid.Nil) = %v, want ErrZeroTenant", err)
	}
}

func TestCatalog_ListByTenant_OrderAndIsolation(t *testing.T) {
	db, ctx := freshDBWithCatalog(t)
	tenantA := seedTenantForCatalog(t, ctx, db, "list-a")
	tenantB := seedTenantForCatalog(t, ctx, db, "list-b")
	store := newCatalogStore(t, db)
	actorID := uuid.New()

	// Insert in reverse chronological order to prove ListByTenant
	// reorders by created_at ASC.
	older := catalogFixedNow
	newer := catalogFixedNow.Add(time.Hour)

	pOlder, _ := catalog.NewProduct(tenantA, "Older", "", 1, nil, older)
	pNewer, _ := catalog.NewProduct(tenantA, "Newer", "", 2, nil, newer)
	pB, _ := catalog.NewProduct(tenantB, "OtherTenant", "", 3, nil, newer)

	if err := store.SaveProduct(ctx, pNewer, actorID); err != nil {
		t.Fatalf("save newer: %v", err)
	}
	if err := store.SaveProduct(ctx, pOlder, actorID); err != nil {
		t.Fatalf("save older: %v", err)
	}
	if err := store.SaveProduct(ctx, pB, actorID); err != nil {
		t.Fatalf("save tenantB: %v", err)
	}

	got, err := store.ListByTenant(ctx, tenantA)
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (tenantB row leaked through RLS?)", len(got))
	}
	if got[0].Name() != "Older" || got[1].Name() != "Newer" {
		t.Errorf("ordering: %s,%s want Older,Newer", got[0].Name(), got[1].Name())
	}
}

func TestCatalog_ListByTenant_ZeroTenant(t *testing.T) {
	db, _ := freshDBWithCatalog(t)
	store := newCatalogStore(t, db)
	_, err := store.ListByTenant(context.Background(), uuid.Nil)
	if !errors.Is(err, catalog.ErrZeroTenant) {
		t.Errorf("ListByTenant(uuid.Nil) = %v, want ErrZeroTenant", err)
	}
}

func TestCatalog_SaveProduct_Upserts(t *testing.T) {
	db, ctx := freshDBWithCatalog(t)
	tenantID := seedTenantForCatalog(t, ctx, db, "upsert")
	store := newCatalogStore(t, db)
	actorID := uuid.New()

	p, _ := catalog.NewProduct(tenantID, "Old", "", 100, []string{"a"}, catalogFixedNow)
	if err := store.SaveProduct(ctx, p, actorID); err != nil {
		t.Fatalf("first save: %v", err)
	}
	later := catalogFixedNow.Add(time.Hour)
	if err := p.Rename("New", later); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := p.SetPrice(200, later); err != nil {
		t.Fatalf("SetPrice: %v", err)
	}
	if err := store.SaveProduct(ctx, p, actorID); err != nil {
		t.Fatalf("second save: %v", err)
	}

	got, err := store.GetByID(ctx, tenantID, p.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name() != "New" || got.PriceCents() != 200 {
		t.Errorf("upsert did not refresh fields: %+v", got)
	}
}

func TestCatalog_SaveProduct_NilProduct(t *testing.T) {
	db, _ := freshDBWithCatalog(t)
	store := newCatalogStore(t, db)
	err := store.SaveProduct(context.Background(), nil, uuid.New())
	if err == nil {
		t.Fatalf("SaveProduct(nil) = nil, want error")
	}
}

func TestCatalog_DeleteProduct_CascadesArguments(t *testing.T) {
	db, ctx := freshDBWithCatalog(t)
	tenantID := seedTenantForCatalog(t, ctx, db, "delete")
	store := newCatalogStore(t, db)
	actorID := uuid.New()

	p, _ := catalog.NewProduct(tenantID, "Plan", "", 100, nil, catalogFixedNow)
	if err := store.SaveProduct(ctx, p, actorID); err != nil {
		t.Fatalf("save product: %v", err)
	}
	a, _ := catalog.NewProductArgument(tenantID, p.ID(),
		catalog.ScopeAnchor{Type: catalog.ScopeTenant, ID: tenantID.String()},
		"pitch", catalogFixedNow)
	if err := store.SaveArgument(ctx, a, actorID); err != nil {
		t.Fatalf("save argument: %v", err)
	}

	if err := store.DeleteProduct(ctx, tenantID, p.ID(), actorID); err != nil {
		t.Fatalf("DeleteProduct: %v", err)
	}

	// Argument should be gone via ON DELETE CASCADE.
	args, err := store.ListByProduct(ctx, tenantID, p.ID())
	if err != nil {
		t.Fatalf("ListByProduct: %v", err)
	}
	if len(args) != 0 {
		t.Errorf("after DeleteProduct: %d arguments, want 0 (CASCADE)", len(args))
	}
}

func TestCatalog_DeleteProduct_NotFound(t *testing.T) {
	db, ctx := freshDBWithCatalog(t)
	tenantID := seedTenantForCatalog(t, ctx, db, "del-404")
	store := newCatalogStore(t, db)
	err := store.DeleteProduct(ctx, tenantID, uuid.New(), uuid.New())
	if !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("DeleteProduct(missing) = %v, want ErrNotFound", err)
	}
}

func TestCatalog_DeleteProduct_ZeroTenant(t *testing.T) {
	db, _ := freshDBWithCatalog(t)
	store := newCatalogStore(t, db)
	err := store.DeleteProduct(context.Background(), uuid.Nil, uuid.New(), uuid.New())
	if !errors.Is(err, catalog.ErrZeroTenant) {
		t.Errorf("DeleteProduct(uuid.Nil) = %v, want ErrZeroTenant", err)
	}
}

// ---------------------------------------------------------------------------
// ArgumentRepository
// ---------------------------------------------------------------------------

func TestCatalog_SaveAndListArguments(t *testing.T) {
	db, ctx := freshDBWithCatalog(t)
	tenantID := seedTenantForCatalog(t, ctx, db, "args")
	store := newCatalogStore(t, db)
	actorID := uuid.New()

	p, _ := catalog.NewProduct(tenantID, "Plan", "", 100, nil, catalogFixedNow)
	if err := store.SaveProduct(ctx, p, actorID); err != nil {
		t.Fatalf("save product: %v", err)
	}

	teamID := uuid.New().String()
	args := []*catalog.ProductArgument{
		mustArg(t, tenantID, p.ID(), catalog.ScopeTenant, tenantID.String(), "tenant-pitch"),
		mustArg(t, tenantID, p.ID(), catalog.ScopeTeam, teamID, "team-pitch"),
		mustArg(t, tenantID, p.ID(), catalog.ScopeChannel, "whatsapp", "channel-pitch"),
	}
	for _, a := range args {
		if err := store.SaveArgument(ctx, a, actorID); err != nil {
			t.Fatalf("SaveArgument: %v", err)
		}
	}

	got, err := store.ListByProduct(ctx, tenantID, p.ID())
	if err != nil {
		t.Fatalf("ListByProduct: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// ORDER BY scope_type, scope_id, created_at →
	// channel, team, tenant (alphabetical on scope_type).
	if got[0].Anchor().Type != catalog.ScopeChannel ||
		got[1].Anchor().Type != catalog.ScopeTeam ||
		got[2].Anchor().Type != catalog.ScopeTenant {
		t.Errorf("ORDER BY scope_type broken: %v",
			[]catalog.ScopeType{got[0].Anchor().Type, got[1].Anchor().Type, got[2].Anchor().Type})
	}
}

func TestCatalog_SaveArgument_DuplicateScope(t *testing.T) {
	db, ctx := freshDBWithCatalog(t)
	tenantID := seedTenantForCatalog(t, ctx, db, "dup")
	store := newCatalogStore(t, db)
	actorID := uuid.New()

	p, _ := catalog.NewProduct(tenantID, "Plan", "", 0, nil, catalogFixedNow)
	if err := store.SaveProduct(ctx, p, actorID); err != nil {
		t.Fatalf("save product: %v", err)
	}

	first := mustArg(t, tenantID, p.ID(), catalog.ScopeChannel, "whatsapp", "first")
	if err := store.SaveArgument(ctx, first, actorID); err != nil {
		t.Fatalf("first save: %v", err)
	}

	second := mustArg(t, tenantID, p.ID(), catalog.ScopeChannel, "whatsapp", "second")
	err := store.SaveArgument(ctx, second, actorID)
	if !errors.Is(err, catalog.ErrDuplicateArgument) {
		t.Errorf("second save: %v, want ErrDuplicateArgument", err)
	}
}

func TestCatalog_SaveArgument_NilArgument(t *testing.T) {
	db, _ := freshDBWithCatalog(t)
	store := newCatalogStore(t, db)
	err := store.SaveArgument(context.Background(), nil, uuid.New())
	if err == nil {
		t.Fatalf("SaveArgument(nil) = nil, want error")
	}
}

func TestCatalog_SaveArgument_UpsertOnID(t *testing.T) {
	db, ctx := freshDBWithCatalog(t)
	tenantID := seedTenantForCatalog(t, ctx, db, "arg-upsert")
	store := newCatalogStore(t, db)
	actorID := uuid.New()

	p, _ := catalog.NewProduct(tenantID, "Plan", "", 0, nil, catalogFixedNow)
	if err := store.SaveProduct(ctx, p, actorID); err != nil {
		t.Fatalf("save product: %v", err)
	}
	a := mustArg(t, tenantID, p.ID(), catalog.ScopeTenant, tenantID.String(), "old")
	if err := store.SaveArgument(ctx, a, actorID); err != nil {
		t.Fatalf("first save: %v", err)
	}
	later := catalogFixedNow.Add(time.Hour)
	if err := a.Rewrite("new", later); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if err := store.SaveArgument(ctx, a, actorID); err != nil {
		t.Fatalf("second save: %v", err)
	}

	got, err := store.ListByProduct(ctx, tenantID, p.ID())
	if err != nil {
		t.Fatalf("ListByProduct: %v", err)
	}
	if len(got) != 1 || got[0].Text() != "new" {
		t.Errorf("upsert did not refresh text: %+v", got)
	}
}

func TestCatalog_ListByProduct_ZeroTenant(t *testing.T) {
	db, _ := freshDBWithCatalog(t)
	store := newCatalogStore(t, db)
	_, err := store.ListByProduct(context.Background(), uuid.Nil, uuid.New())
	if !errors.Is(err, catalog.ErrZeroTenant) {
		t.Errorf("ListByProduct(uuid.Nil) = %v, want ErrZeroTenant", err)
	}
}

func TestCatalog_DeleteArgument(t *testing.T) {
	db, ctx := freshDBWithCatalog(t)
	tenantID := seedTenantForCatalog(t, ctx, db, "arg-del")
	store := newCatalogStore(t, db)
	actorID := uuid.New()

	p, _ := catalog.NewProduct(tenantID, "Plan", "", 0, nil, catalogFixedNow)
	if err := store.SaveProduct(ctx, p, actorID); err != nil {
		t.Fatalf("save product: %v", err)
	}
	a := mustArg(t, tenantID, p.ID(), catalog.ScopeTenant, tenantID.String(), "x")
	if err := store.SaveArgument(ctx, a, actorID); err != nil {
		t.Fatalf("save argument: %v", err)
	}

	if err := store.DeleteArgument(ctx, tenantID, a.ID(), actorID); err != nil {
		t.Fatalf("DeleteArgument: %v", err)
	}
	args, err := store.ListByProduct(ctx, tenantID, p.ID())
	if err != nil {
		t.Fatalf("ListByProduct: %v", err)
	}
	if len(args) != 0 {
		t.Errorf("after DeleteArgument: %d args, want 0", len(args))
	}

	// Re-delete must surface ErrNotFound.
	if err := store.DeleteArgument(ctx, tenantID, a.ID(), actorID); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("DeleteArgument(missing) = %v, want ErrNotFound", err)
	}
}

func TestCatalog_DeleteArgument_ZeroTenant(t *testing.T) {
	db, _ := freshDBWithCatalog(t)
	store := newCatalogStore(t, db)
	err := store.DeleteArgument(context.Background(), uuid.Nil, uuid.New(), uuid.New())
	if !errors.Is(err, catalog.ErrZeroTenant) {
		t.Errorf("DeleteArgument(uuid.Nil) = %v, want ErrZeroTenant", err)
	}
}

// ---------------------------------------------------------------------------
// Tenant isolation through the adapter (defense-in-depth on top of RLS).
// ---------------------------------------------------------------------------

// TestCatalog_TenantIsolation_GetByID_RejectsCrossTenant proves the
// adapter's explicit tenant predicate (NOT just RLS) refuses to
// surface another tenant's product, even when the runtime pool is
// scoped to tenant A and we hand it B's id.
func TestCatalog_TenantIsolation_GetByID_RejectsCrossTenant(t *testing.T) {
	db, ctx := freshDBWithCatalog(t)
	tenantA := seedTenantForCatalog(t, ctx, db, "iso-a")
	tenantB := seedTenantForCatalog(t, ctx, db, "iso-b")
	store := newCatalogStore(t, db)
	actorID := uuid.New()

	pB, _ := catalog.NewProduct(tenantB, "B-plan", "", 100, nil, catalogFixedNow)
	if err := store.SaveProduct(ctx, pB, actorID); err != nil {
		t.Fatalf("save tenantB product: %v", err)
	}

	// Asking tenantA's pool for tenantB's product must be ErrNotFound,
	// not a leak.
	_, err := store.GetByID(ctx, tenantA, pB.ID())
	if !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("GetByID(A, B.id) = %v, want ErrNotFound (cross-tenant leak)", err)
	}
}

func mustArg(t *testing.T, tenantID, productID uuid.UUID, st catalog.ScopeType, id, text string) *catalog.ProductArgument {
	t.Helper()
	a, err := catalog.NewProductArgument(tenantID, productID,
		catalog.ScopeAnchor{Type: st, ID: id}, text, catalogFixedNow)
	if err != nil {
		t.Fatalf("NewProductArgument(%s/%s): %v", st, id, err)
	}
	return a
}
