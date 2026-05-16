package catalog_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/catalog"
)

func TestScopeType_Valid(t *testing.T) {
	cases := []struct {
		st   catalog.ScopeType
		want bool
	}{
		{catalog.ScopeTenant, true},
		{catalog.ScopeTeam, true},
		{catalog.ScopeChannel, true},
		{catalog.ScopeType("global"), false},
		{catalog.ScopeType(""), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.st)+"/valid", func(t *testing.T) {
			if got := tc.st.Valid(); got != tc.want {
				t.Errorf("Valid(%q) = %v, want %v", tc.st, got, tc.want)
			}
		})
	}
}

func TestScopeAnchor_Validate(t *testing.T) {
	cases := []struct {
		name string
		s    catalog.ScopeAnchor
		want error
	}{
		{"ok tenant", catalog.ScopeAnchor{Type: catalog.ScopeTenant, ID: "x"}, nil},
		{"bad type", catalog.ScopeAnchor{Type: "global", ID: "x"}, catalog.ErrInvalidScope},
		{"empty id", catalog.ScopeAnchor{Type: catalog.ScopeTenant, ID: ""}, catalog.ErrInvalidScope},
		{"whitespace id", catalog.ScopeAnchor{Type: catalog.ScopeChannel, ID: "  "}, catalog.ErrInvalidScope},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.Validate()
			if tc.want == nil && err != nil {
				t.Errorf("Validate: %v, want nil", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("Validate: %v, want errors.Is(%v)", err, tc.want)
			}
		})
	}
}

func TestNewProductArgument_Valid(t *testing.T) {
	tenantID := uuid.New()
	productID := uuid.New()
	anchor := catalog.ScopeAnchor{Type: catalog.ScopeChannel, ID: "whatsapp"}
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	a, err := catalog.NewProductArgument(tenantID, productID, anchor, "buy now", now)
	if err != nil {
		t.Fatalf("NewProductArgument: %v", err)
	}
	if a.TenantID() != tenantID || a.ProductID() != productID {
		t.Errorf("ids not preserved: tenant=%s product=%s", a.TenantID(), a.ProductID())
	}
	if a.Anchor() != anchor {
		t.Errorf("Anchor = %+v, want %+v", a.Anchor(), anchor)
	}
	if a.Text() != "buy now" {
		t.Errorf("Text = %q", a.Text())
	}
	if !a.CreatedAt().Equal(now) || !a.UpdatedAt().Equal(now) {
		t.Errorf("timestamps not pinned to now")
	}
	if a.ID() == uuid.Nil {
		t.Errorf("ID is uuid.Nil")
	}
}

func TestNewProductArgument_Invariants(t *testing.T) {
	tenantID := uuid.New()
	productID := uuid.New()
	good := catalog.ScopeAnchor{Type: catalog.ScopeTenant, ID: tenantID.String()}
	cases := []struct {
		name      string
		tenantID  uuid.UUID
		productID uuid.UUID
		anchor    catalog.ScopeAnchor
		text      string
		want      error
	}{
		{"nil tenant", uuid.Nil, productID, good, "x", catalog.ErrZeroTenant},
		{"nil product", tenantID, uuid.Nil, good, "x", catalog.ErrInvalidArgument},
		{"bad scope", tenantID, productID, catalog.ScopeAnchor{Type: "global", ID: "x"}, "x", catalog.ErrInvalidScope},
		{"empty text", tenantID, productID, good, "", catalog.ErrInvalidArgument},
		{"whitespace text", tenantID, productID, good, "   ", catalog.ErrInvalidArgument},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := catalog.NewProductArgument(tc.tenantID, tc.productID, tc.anchor, tc.text, fixedNow)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want errors.Is(%v)", err, tc.want)
			}
		})
	}
}

func TestProductArgumentRewrite(t *testing.T) {
	a, _ := catalog.NewProductArgument(uuid.New(), uuid.New(),
		catalog.ScopeAnchor{Type: catalog.ScopeTenant, ID: "t"}, "old", fixedNow)
	later := fixedNow.Add(2 * time.Hour)
	if err := a.Rewrite("new", later); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if a.Text() != "new" {
		t.Errorf("Text = %q, want %q", a.Text(), "new")
	}
	if !a.UpdatedAt().Equal(later) {
		t.Errorf("UpdatedAt not bumped on Rewrite")
	}
	if err := a.Rewrite("   ", later); !errors.Is(err, catalog.ErrInvalidArgument) {
		t.Errorf("Rewrite blank: %v, want ErrInvalidArgument", err)
	}
}

func TestHydrateProductArgument_RoundTrip(t *testing.T) {
	id := uuid.New()
	tenantID := uuid.New()
	productID := uuid.New()
	anchor := catalog.ScopeAnchor{Type: catalog.ScopeTeam, ID: uuid.New().String()}
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	a := catalog.HydrateProductArgument(id, tenantID, productID, anchor, "txt", now, now)
	if a.ID() != id || a.TenantID() != tenantID || a.ProductID() != productID {
		t.Errorf("ids not preserved: %+v", a)
	}
	if a.Anchor() != anchor {
		t.Errorf("Anchor: %+v, want %+v", a.Anchor(), anchor)
	}
}
