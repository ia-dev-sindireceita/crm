package catalog_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/catalog"
)

var fixedNow = time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

func TestNewProduct_Valid(t *testing.T) {
	tenantID := uuid.New()
	p, err := catalog.NewProduct(tenantID, "Plan Pro", "the pro plan", 4990,
		[]string{"saas", "monthly"}, fixedNow)
	if err != nil {
		t.Fatalf("NewProduct: %v", err)
	}
	if p.TenantID() != tenantID {
		t.Errorf("TenantID = %s, want %s", p.TenantID(), tenantID)
	}
	if p.Name() != "Plan Pro" {
		t.Errorf("Name = %q, want %q", p.Name(), "Plan Pro")
	}
	if p.Description() != "the pro plan" {
		t.Errorf("Description = %q", p.Description())
	}
	if p.PriceCents() != 4990 {
		t.Errorf("PriceCents = %d, want 4990", p.PriceCents())
	}
	got := p.Tags()
	if len(got) != 2 || got[0] != "saas" || got[1] != "monthly" {
		t.Errorf("Tags = %v, want [saas monthly]", got)
	}
	if !p.CreatedAt().Equal(fixedNow) || !p.UpdatedAt().Equal(fixedNow) {
		t.Errorf("timestamps not pinned to fixedNow")
	}
	if p.ID() == uuid.Nil {
		t.Errorf("ID is uuid.Nil")
	}
}

func TestNewProduct_Invariants(t *testing.T) {
	tenantID := uuid.New()
	cases := []struct {
		name        string
		tenantID    uuid.UUID
		productName string
		price       int
		tags        []string
		want        error
	}{
		{"nil tenant", uuid.Nil, "Plan", 100, nil, catalog.ErrZeroTenant},
		{"empty name", tenantID, "", 100, nil, catalog.ErrInvalidProduct},
		{"whitespace name", tenantID, "   ", 100, nil, catalog.ErrInvalidProduct},
		{"negative price", tenantID, "Plan", -1, nil, catalog.ErrInvalidProduct},
		{"blank tag", tenantID, "Plan", 0, []string{"ok", ""}, catalog.ErrInvalidProduct},
		{"whitespace tag", tenantID, "Plan", 0, []string{"  "}, catalog.ErrInvalidProduct},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := catalog.NewProduct(tc.tenantID, tc.productName, "", tc.price, tc.tags, fixedNow)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want errors.Is(%v)", err, tc.want)
			}
		})
	}
}

func TestNewProduct_TagsTrimmed(t *testing.T) {
	p, err := catalog.NewProduct(uuid.New(), "Plan", "", 0,
		[]string{"  saas  ", "monthly"}, fixedNow)
	if err != nil {
		t.Fatalf("NewProduct: %v", err)
	}
	got := p.Tags()
	if got[0] != "saas" {
		t.Errorf("tag[0] = %q, want %q (trimmed)", got[0], "saas")
	}
}

func TestProductTags_DefensiveCopy(t *testing.T) {
	p, _ := catalog.NewProduct(uuid.New(), "P", "", 0, []string{"a", "b"}, fixedNow)
	got := p.Tags()
	got[0] = "MUTATED"
	again := p.Tags()
	if again[0] != "a" {
		t.Errorf("Tags returned a live slice; after caller mutation got %q, want %q", again[0], "a")
	}
}

func TestProductRename(t *testing.T) {
	p, _ := catalog.NewProduct(uuid.New(), "Old", "", 0, nil, fixedNow)
	later := fixedNow.Add(time.Hour)
	if err := p.Rename("New", later); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if p.Name() != "New" {
		t.Errorf("Name = %q after Rename, want %q", p.Name(), "New")
	}
	if !p.UpdatedAt().Equal(later) {
		t.Errorf("UpdatedAt not bumped on Rename")
	}
	if err := p.Rename("  ", later); !errors.Is(err, catalog.ErrInvalidProduct) {
		t.Errorf("Rename blank: %v, want ErrInvalidProduct", err)
	}
}

func TestProductSetPrice(t *testing.T) {
	p, _ := catalog.NewProduct(uuid.New(), "Plan", "", 100, nil, fixedNow)
	later := fixedNow.Add(time.Hour)
	if err := p.SetPrice(200, later); err != nil {
		t.Fatalf("SetPrice: %v", err)
	}
	if p.PriceCents() != 200 {
		t.Errorf("PriceCents = %d, want 200", p.PriceCents())
	}
	if !p.UpdatedAt().Equal(later) {
		t.Errorf("UpdatedAt not bumped on SetPrice")
	}
	if err := p.SetPrice(-1, later); !errors.Is(err, catalog.ErrInvalidProduct) {
		t.Errorf("SetPrice negative: %v, want ErrInvalidProduct", err)
	}
}

func TestHydrateProduct_DefensiveCopy(t *testing.T) {
	tags := []string{"a", "b"}
	p := catalog.HydrateProduct(uuid.New(), uuid.New(), "P", "", 0, tags, fixedNow, fixedNow)
	tags[0] = "MUTATED"
	got := p.Tags()
	if got[0] != "a" {
		t.Errorf("HydrateProduct kept a live reference to caller slice; got %q want %q",
			got[0], "a")
	}
}
