package catalog_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/catalog"
)

func TestProduct_Category_DefaultEmpty(t *testing.T) {
	p, err := catalog.NewProduct(uuid.New(), "Plan", "", 0, nil, fixedNow)
	if err != nil {
		t.Fatalf("NewProduct: %v", err)
	}
	if got := p.Category(); got != "" {
		t.Errorf("Category() = %q, want empty for new product", got)
	}
}

func TestProduct_SetCategory(t *testing.T) {
	p, _ := catalog.NewProduct(uuid.New(), "Plan", "", 0, nil, fixedNow)
	later := fixedNow.Add(time.Hour)
	if err := p.SetCategory("Assinaturas", later); err != nil {
		t.Fatalf("SetCategory: %v", err)
	}
	if p.Category() != "Assinaturas" {
		t.Errorf("Category() = %q, want %q", p.Category(), "Assinaturas")
	}
	if !p.UpdatedAt().Equal(later) {
		t.Errorf("UpdatedAt not bumped on SetCategory")
	}
}

func TestProduct_SetCategory_Trims(t *testing.T) {
	p, _ := catalog.NewProduct(uuid.New(), "Plan", "", 0, nil, fixedNow)
	if err := p.SetCategory("  spaced  ", fixedNow); err != nil {
		t.Fatalf("SetCategory: %v", err)
	}
	if p.Category() != "spaced" {
		t.Errorf("Category() = %q, want %q (trimmed)", p.Category(), "spaced")
	}
}

func TestProduct_SetCategory_Empty_Clears(t *testing.T) {
	p, _ := catalog.NewProduct(uuid.New(), "Plan", "", 0, nil, fixedNow)
	if err := p.SetCategory("First", fixedNow); err != nil {
		t.Fatalf("SetCategory: %v", err)
	}
	if err := p.SetCategory("   ", fixedNow); err != nil {
		t.Errorf("SetCategory empty rejected: %v; want clearing semantics", err)
	}
	if p.Category() != "" {
		t.Errorf("Category() = %q, want \"\" after clear", p.Category())
	}
}

func TestProduct_SetCategory_TooLong(t *testing.T) {
	p, _ := catalog.NewProduct(uuid.New(), "Plan", "", 0, nil, fixedNow)
	tooLong := strings.Repeat("a", catalog.MaxCategoryLen+1)
	if err := p.SetCategory(tooLong, fixedNow); !errors.Is(err, catalog.ErrInvalidProduct) {
		t.Errorf("SetCategory(too long): %v, want ErrInvalidProduct", err)
	}
}

func TestHydrateProductFull_Roundtrip(t *testing.T) {
	id := uuid.New()
	tenantID := uuid.New()
	p := catalog.HydrateProductFull(id, tenantID, "Plan", "desc", 1234,
		[]string{"a", "b"}, "Assinaturas", fixedNow, fixedNow)
	if p.ID() != id {
		t.Errorf("ID = %s, want %s", p.ID(), id)
	}
	if p.Category() != "Assinaturas" {
		t.Errorf("Category = %q, want %q", p.Category(), "Assinaturas")
	}
}

func TestHydrateProduct_LegacyDefaultsToEmptyCategory(t *testing.T) {
	p := catalog.HydrateProduct(uuid.New(), uuid.New(), "P", "", 0, nil, fixedNow, fixedNow)
	if p.Category() != "" {
		t.Errorf("Category = %q, want empty for legacy HydrateProduct", p.Category())
	}
}
