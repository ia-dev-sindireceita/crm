package main

// SIN-63963 / UX-F4 — covers the iamAdapter.LoadBranding delegation that
// lets the usermfa wire layer pick up the branding read port via an
// optional interface assertion. The method just forwards to the shared
// TenantResolver, so a scanning stub is enough to prove the wiring.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// brandingStubRow scans the two tenantBrandingSQL columns.
type brandingStubRow struct {
	logoURL    string
	whiteLabel bool
	err        error
}

func (r brandingStubRow) Scan(dst ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dst[0].(*string)) = r.logoURL
	*(dst[1].(*bool)) = r.whiteLabel
	return nil
}

type brandingStubQuerier struct{ row pgx.Row }

func (q brandingStubQuerier) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return q.row
}

func TestIAMAdapter_LoadBranding_DelegatesToResolver(t *testing.T) {
	t.Parallel()
	resolver, err := postgresadapter.NewTenantResolver(brandingStubQuerier{row: brandingStubRow{
		logoURL:    "https://static.example/t/acme/logo.svg",
		whiteLabel: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	a := iamAdapter{tenants: resolver}

	b, err := a.LoadBranding(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("LoadBranding err = %v", err)
	}
	if b.LogoURL != "https://static.example/t/acme/logo.svg" || !b.WhiteLabel {
		t.Fatalf("branding = %+v, want logo+whitelabel", b)
	}

	// iamAdapter must satisfy tenancy.BrandingReader so the usermfa wire's
	// optional assertion lights up in production.
	var _ tenancy.BrandingReader = iamAdapter{}
}

func TestIAMAdapter_LoadBranding_PropagatesNotFound(t *testing.T) {
	t.Parallel()
	resolver, err := postgresadapter.NewTenantResolver(brandingStubQuerier{row: brandingStubRow{err: pgx.ErrNoRows}})
	if err != nil {
		t.Fatal(err)
	}
	a := iamAdapter{tenants: resolver}

	_, err = a.LoadBranding(context.Background(), uuid.New())
	if !errors.Is(err, tenancy.ErrTenantNotFound) {
		t.Fatalf("err = %v, want ErrTenantNotFound", err)
	}
}
