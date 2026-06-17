package postgres_test

// SIN-63946 adapter tests for the product.category round-trip
// (migration 0118). Sibling of catalog_adapter_test.go; reuses the
// freshDBWithCatalog harness so the same 0098 + 0118 chain applies.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/catalog"
)

func TestCatalog_Category_RoundTrip(t *testing.T) {
	db, ctx := freshDBWithCatalog(t)
	tenantID := seedTenantForCatalog(t, ctx, db, "cat")
	store := newCatalogStore(t, db)
	actorID := uuid.New()

	p, err := catalog.NewProduct(tenantID, "Plan Pro", "the pro plan", 4990,
		[]string{"saas"}, time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewProduct: %v", err)
	}
	if err := p.SetCategory("Assinaturas", time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("SetCategory: %v", err)
	}
	if err := store.SaveProduct(ctx, p, actorID); err != nil {
		t.Fatalf("SaveProduct: %v", err)
	}

	got, err := store.GetByID(ctx, tenantID, p.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Category() != "Assinaturas" {
		t.Errorf("Category() = %q, want %q", got.Category(), "Assinaturas")
	}
}

func TestCatalog_Category_DefaultsToEmpty(t *testing.T) {
	db, ctx := freshDBWithCatalog(t)
	tenantID := seedTenantForCatalog(t, ctx, db, "catnull")
	store := newCatalogStore(t, db)
	actorID := uuid.New()

	p, err := catalog.NewProduct(tenantID, "Plan", "", 0, nil,
		time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC))
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
	if got.Category() != "" {
		t.Errorf("Category() = %q, want \"\"", got.Category())
	}
}

func TestCatalog_Category_UpdateOverwrites(t *testing.T) {
	db, ctx := freshDBWithCatalog(t)
	tenantID := seedTenantForCatalog(t, ctx, db, "catupd")
	store := newCatalogStore(t, db)
	actorID := uuid.New()

	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	p, err := catalog.NewProduct(tenantID, "Plan", "", 0, nil, now)
	if err != nil {
		t.Fatalf("NewProduct: %v", err)
	}
	if err := p.SetCategory("First", now); err != nil {
		t.Fatalf("SetCategory first: %v", err)
	}
	if err := store.SaveProduct(ctx, p, actorID); err != nil {
		t.Fatalf("SaveProduct: %v", err)
	}
	// upsert with new category
	updated := catalog.HydrateProductFull(p.ID(), p.TenantID(), p.Name(), p.Description(),
		p.PriceCents(), p.Tags(), "Second", p.CreatedAt(), now.Add(time.Hour))
	if err := store.SaveProduct(ctx, updated, actorID); err != nil {
		t.Fatalf("SaveProduct update: %v", err)
	}

	got, err := store.GetByID(ctx, tenantID, p.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Category() != "Second" {
		t.Errorf("Category() = %q, want %q", got.Category(), "Second")
	}
}

func TestCatalog_Category_IndexExists(t *testing.T) {
	db, ctx := freshDBWithCatalog(t)
	var name string
	err := db.AdminPool().QueryRow(ctx,
		`SELECT indexname FROM pg_indexes
		  WHERE schemaname = 'public'
		    AND tablename  = 'product'
		    AND indexname  = 'product_tenant_category_idx'`).Scan(&name)
	if err != nil {
		t.Fatalf("expected product_tenant_category_idx after 0118; err=%v", err)
	}
	if name != "product_tenant_category_idx" {
		t.Errorf("indexname = %q, want %q", name, "product_tenant_category_idx")
	}
}

// silence unused if context import drops
var _ = context.Background
