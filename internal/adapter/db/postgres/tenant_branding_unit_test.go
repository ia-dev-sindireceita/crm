package postgres_test

// SIN-63963 / UX-F4 — unit tests for the LoadBranding adapter method.
// Drives the error paths (zero id, no rows, transient failure) and the
// happy path through a scanning stub so coverage stays above 85% without
// spinning Postgres for this package (mirrors tenant_privacy_unit_test).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// brandingRow scans the two branding columns into the destination
// pointers. The shape matches tenantBrandingSQL.
type brandingRow struct {
	logoURL    string
	whiteLabel bool
}

func (r brandingRow) Scan(dst ...any) error {
	if got := len(dst); got != 2 {
		return errors.New("scan dst count mismatch")
	}
	*(dst[0].(*string)) = r.logoURL
	*(dst[1].(*bool)) = r.whiteLabel
	return nil
}

func TestLoadBranding_HappyPath(t *testing.T) {
	t.Parallel()
	r, err := postgresadapter.NewTenantResolver(stubQuerier{row: brandingRow{
		logoURL:    "https://static.example/t/acme/logo.svg",
		whiteLabel: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.LoadBranding(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if b.LogoURL != "https://static.example/t/acme/logo.svg" {
		t.Errorf("LogoURL = %q", b.LogoURL)
	}
	if !b.WhiteLabel {
		t.Errorf("WhiteLabel = %v, want true", b.WhiteLabel)
	}
}

func TestLoadBranding_EmptyLogoIsValid(t *testing.T) {
	t.Parallel()
	r, err := postgresadapter.NewTenantResolver(stubQuerier{row: brandingRow{}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.LoadBranding(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if b.LogoURL != "" {
		t.Errorf("LogoURL = %q, want empty", b.LogoURL)
	}
	if b.WhiteLabel {
		t.Errorf("WhiteLabel = %v, want false", b.WhiteLabel)
	}
}

func TestLoadBranding_ZeroIDFails(t *testing.T) {
	t.Parallel()
	r, err := postgresadapter.NewTenantResolver(stubQuerier{row: stubRow{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.LoadBranding(context.Background(), uuid.Nil)
	if !errors.Is(err, tenancy.ErrTenantNotFound) {
		t.Fatalf("err = %v; want ErrTenantNotFound", err)
	}
}

func TestLoadBranding_NoRowsMapsToNotFound(t *testing.T) {
	t.Parallel()
	r, err := postgresadapter.NewTenantResolver(stubQuerier{row: stubRow{err: pgx.ErrNoRows}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.LoadBranding(context.Background(), uuid.New())
	if !errors.Is(err, tenancy.ErrTenantNotFound) {
		t.Fatalf("err = %v; want ErrTenantNotFound", err)
	}
}

func TestLoadBranding_TransientErrorWraps(t *testing.T) {
	t.Parallel()
	transient := errors.New("connection reset by peer")
	r, err := postgresadapter.NewTenantResolver(stubQuerier{row: stubRow{err: transient}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.LoadBranding(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, transient) {
		t.Errorf("err = %v; want wraps %v", err, transient)
	}
	if !strings.Contains(err.Error(), "branding") {
		t.Errorf("err = %q; want context prefix", err.Error())
	}
}

// TestLoadBranding_NilReceiverGuards covers the defensive nil-pool guard
// so a misconstructed resolver surfaces ErrNilPool rather than panicking.
func TestLoadBranding_NilReceiverGuards(t *testing.T) {
	t.Parallel()
	var r *postgresadapter.TenantResolver
	_, err := r.LoadBranding(context.Background(), uuid.New())
	if !errors.Is(err, postgresadapter.ErrNilPool) {
		t.Fatalf("err = %v; want ErrNilPool", err)
	}
}
