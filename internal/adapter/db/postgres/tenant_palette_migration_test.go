package postgres_test

// SIN-63075 Fase 5 — White-label avançado: smoke test for
// 0104_tenant_palette. Applies 0004 → 0104 on top of the harness default
// 0001-0003 chain. Covers up/down/up round-trip + down idempotency,
// CHECK constraints (source enum, hex format), tenant-cascade delete,
// and the canonical four-policy RLS posture against app_runtime.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

const tenantPaletteTable = "tenant_palette"

// freshDBWithTenantPalette applies 0004 → 0104 to a fresh DB.
func freshDBWithTenantPalette(t *testing.T) (*testpg.DB, context.Context) {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0104_tenant_palette.up.sql",
	)
	return db, ctx
}

func tenantPalettePresent(t *testing.T, ctx context.Context, db *testpg.DB) bool {
	t.Helper()
	var count int
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relname = $1 AND n.nspname = 'public'`,
		tenantPaletteTable).Scan(&count); err != nil {
		t.Fatalf("table-exists probe: %v", err)
	}
	return count == 1
}

func insertTenant(t *testing.T, ctx context.Context, db *testpg.DB) uuid.UUID {
	t.Helper()
	tid := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tid, "t-"+tid.String()[:8], fmt.Sprintf("t-%s.crm.local", tid.String()[:8])); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return tid
}

func insertPalette(ctx context.Context, db *testpg.DB, tid uuid.UUID, src, primary string) error {
	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenant_palette
		   (tenant_id, primary_color, secondary_color, accent_color,
		    foreground_color, background_color, text_on_primary, source)
		 VALUES ($1, $2, '#222222', '#333333', '#0f1115', '#ffffff', '#ffffff', $3)`,
		tid, primary, src)
	return err
}

// TestTenantPalette_UpDownUp round-trips 0104 — up, down, up, down twice —
// asserting the table comes and goes cleanly and that down is idempotent.
func TestTenantPalette_UpDownUp(t *testing.T) {
	db, ctx := freshDBWithTenantPalette(t)

	if !tenantPalettePresent(t, ctx, db) {
		t.Fatalf("after initial up: tenant_palette missing")
	}
	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0104_tenant_palette.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0104_tenant_palette.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if tenantPalettePresent(t, ctx, db) {
		t.Fatalf("after down: tenant_palette still present")
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if !tenantPalettePresent(t, ctx, db) {
		t.Fatalf("after re-up: tenant_palette missing")
	}
	// Down idempotency: applying down twice in a row must not error.
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down #2: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down #3 (idempotent): %v", err)
	}
}

// TestTenantPalette_SourceEnumCheck rejects sources outside the closed
// {extracted, fallback, manual, unknown} set.
func TestTenantPalette_SourceEnumCheck(t *testing.T) {
	db, ctx := freshDBWithTenantPalette(t)
	tid := insertTenant(t, ctx, db)

	if err := insertPalette(ctx, db, tid, "garbage", "#aabbcc"); err == nil {
		t.Fatalf("insert with garbage source: want CHECK violation, got nil")
	} else if !strings.Contains(err.Error(), "tenant_palette_source_chk") {
		t.Fatalf("insert with garbage source: want source-chk violation, got %v", err)
	}

	if err := insertPalette(ctx, db, tid, "extracted", "#aabbcc"); err != nil {
		t.Fatalf("insert with valid source: %v", err)
	}
}

// TestTenantPalette_HexFormatCheck rejects colour columns that are not
// lowercase "#rrggbb". This is the boundary that catches a writer
// forgetting to call branding.RGB.Hex().
func TestTenantPalette_HexFormatCheck(t *testing.T) {
	db, ctx := freshDBWithTenantPalette(t)
	tid := insertTenant(t, ctx, db)

	cases := []struct {
		name    string
		primary string
		want    string
	}{
		{"uppercase", "#AABBCC", "tenant_palette_primary_hex_chk"},
		{"non-hex-digit", "#aabbcz", "tenant_palette_primary_hex_chk"},
		{"missing-hash", "aabbcca", "tenant_palette_primary_hex_chk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := insertPalette(ctx, db, tid, "extracted", tc.primary)
			if err == nil {
				t.Fatalf("insert with primary %q: want CHECK violation, got nil", tc.primary)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("insert with primary %q: want %s violation, got %v", tc.primary, tc.want, err)
			}
		})
	}

	if err := insertPalette(ctx, db, tid, "extracted", "#aabbcc"); err != nil {
		t.Fatalf("insert with valid primary: %v", err)
	}
}

// TestTenantPalette_TenantCascadeDelete deletes the parent tenant row and
// asserts the palette row is removed via ON DELETE CASCADE.
func TestTenantPalette_TenantCascadeDelete(t *testing.T) {
	db, ctx := freshDBWithTenantPalette(t)
	tid := insertTenant(t, ctx, db)
	if err := insertPalette(ctx, db, tid, "extracted", "#aabbcc"); err != nil {
		t.Fatalf("seed palette: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tid); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	var cnt int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM tenant_palette WHERE tenant_id = $1`, tid).Scan(&cnt); err != nil {
		t.Fatalf("count palettes: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("after tenant delete: %d palette rows remain (want 0)", cnt)
	}
}

// TestTenantPalette_RLSPosture exercises the canonical four-policy RLS
// template against app_runtime: a tenant sees its own row, cannot see
// another tenant's row, cannot insert under a different tenant_id, and
// cannot update / delete a row that does not belong to its session
// tenant.
func TestTenantPalette_RLSPosture(t *testing.T) {
	db, ctx := freshDBWithTenantPalette(t)
	tenantA := insertTenant(t, ctx, db)
	tenantB := insertTenant(t, ctx, db)
	if err := insertPalette(ctx, db, tenantA, "extracted", "#aaaaaa"); err != nil {
		t.Fatalf("seed palette A: %v", err)
	}
	if err := insertPalette(ctx, db, tenantB, "extracted", "#bbbbbb"); err != nil {
		t.Fatalf("seed palette B: %v", err)
	}

	// SELECT — tenant A sees only its own row.
	err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
		var primary string
		if err := tx.QueryRow(ctx,
			`SELECT primary_color FROM tenant_palette`).Scan(&primary); err != nil {
			return fmt.Errorf("select own: %w", err)
		}
		if primary != "#aaaaaa" {
			return fmt.Errorf("own primary: got %q want %q", primary, "#aaaaaa")
		}
		var cnt int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM tenant_palette`).Scan(&cnt); err != nil {
			return fmt.Errorf("count: %w", err)
		}
		if cnt != 1 {
			return fmt.Errorf("RLS leaked: tenant A sees %d rows (want 1)", cnt)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RLS select for tenant A: %v", err)
	}

	// INSERT — tenant A may not write a row stamped with tenant B's id.
	// Return the error from fn so WithTenant rolls back the now-poisoned tx
	// and surfaces the RLS-violation back up.
	err = postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
		_, ierr := tx.Exec(ctx,
			`INSERT INTO tenant_palette
			   (tenant_id, primary_color, secondary_color, accent_color,
			    foreground_color, background_color, text_on_primary, source)
			 VALUES ($1, '#cccccc', '#222222', '#333333', '#0f1115', '#ffffff', '#ffffff', 'extracted')`,
			tenantB)
		return ierr
	})
	if err == nil {
		t.Fatalf("cross-tenant insert: want RLS deny, got nil")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("cross-tenant insert: want PgError, got %T %v", err, err)
	}
	if pgErr.Code != "42501" {
		// 42501 (insufficient_privilege) is what Postgres raises when a
		// WITH CHECK clause refuses the row under FORCE ROW LEVEL SECURITY.
		t.Fatalf("cross-tenant insert: want code 42501, got %s (%s)", pgErr.Code, pgErr.Message)
	}
	if !strings.Contains(strings.ToLower(pgErr.Message), "row-level security") &&
		!strings.Contains(strings.ToLower(pgErr.Message), "row level security") {
		t.Fatalf("cross-tenant insert: want RLS-violation message, got %q", pgErr.Message)
	}

	// UPDATE — tenant A cannot mutate tenant B's row (it is invisible).
	err = postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
		ct, uerr := tx.Exec(ctx,
			`UPDATE tenant_palette SET primary_color = '#eeeeee' WHERE tenant_id = $1`,
			tenantB)
		if uerr != nil {
			return fmt.Errorf("update: %w", uerr)
		}
		if ct.RowsAffected() != 0 {
			return fmt.Errorf("update affected %d rows (want 0)", ct.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RLS cross-tenant update: %v", err)
	}

	// DELETE — tenant A cannot delete tenant B's row.
	err = postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
		ct, derr := tx.Exec(ctx,
			`DELETE FROM tenant_palette WHERE tenant_id = $1`, tenantB)
		if derr != nil {
			return fmt.Errorf("delete: %w", derr)
		}
		if ct.RowsAffected() != 0 {
			return fmt.Errorf("delete affected %d rows (want 0)", ct.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RLS cross-tenant delete: %v", err)
	}

	// Confirm tenant B's row is untouched.
	var bPrimary string
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT primary_color FROM tenant_palette WHERE tenant_id = $1`, tenantB).Scan(&bPrimary); err != nil {
		t.Fatalf("admin read tenant B: %v", err)
	}
	if bPrimary != "#bbbbbb" {
		t.Fatalf("tenant B primary mutated to %q (want %q) — RLS bypass", bPrimary, "#bbbbbb")
	}
}
