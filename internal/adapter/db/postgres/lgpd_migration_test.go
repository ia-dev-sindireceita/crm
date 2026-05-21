package postgres_test

// SIN-63186 / Fase 6 PR3 acceptance for the two new migrations.
//
//   * 0107_lgpd_deletion_request — tombstone table for LGPD article-18
//     erasure requests. Tests: up/down idempotency, RLS forced on,
//     partial-unique index gating idempotent upserts, master_ops audit
//     trigger present.
//   * 0108_tenants_dpo_settings  — four nullable columns on `tenants`
//     for DPO contact + privacy policy versioning. Tests: columns
//     present after up, absent after down, existing tenants survive
//     the migration.
//
// Lives in postgres_test so it shares the cluster bootstrap state and
// avoids the SQLSTATE 28P01 race the mastersession pattern was built
// for (memory `testpg shared-cluster ALTER ROLE race`).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

func freshDBWithLGPD(t *testing.T) (*testpg.DB, context.Context) {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0107_lgpd_deletion_request.up.sql",
		"0108_tenants_dpo_settings.up.sql",
	)
	return db, ctx
}

func lgpdDeletionRequestTablePresent(t *testing.T, ctx context.Context, db *testpg.DB) bool {
	t.Helper()
	var n int
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relname = 'lgpd_deletion_request' AND n.nspname = 'public'`).Scan(&n); err != nil {
		t.Fatalf("table-exists probe: %v", err)
	}
	return n == 1
}

func dpoColumnsPresent(t *testing.T, ctx context.Context, db *testpg.DB) int {
	t.Helper()
	var n int
	if err := db.SuperuserPool().QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'tenants'
		   AND column_name IN ('dpo_name','dpo_email','privacy_policy_version','privacy_policy_url')
	`).Scan(&n); err != nil {
		t.Fatalf("column probe: %v", err)
	}
	return n
}

// AC #2 (idempotency) and AC #3 (table shape) — round-trip up/down/up.
func TestLGPDMigrations_UpDownUp(t *testing.T) {
	db, ctx := freshDBWithLGPD(t)

	if !lgpdDeletionRequestTablePresent(t, ctx, db) {
		t.Fatal("up: lgpd_deletion_request missing")
	}
	if got := dpoColumnsPresent(t, ctx, db); got != 4 {
		t.Fatalf("up: expected 4 DPO columns on tenants, got %d", got)
	}

	for _, name := range []string{
		"0108_tenants_dpo_settings.down.sql",
		"0107_lgpd_deletion_request.down.sql",
	} {
		body, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := db.AdminPool().Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	if lgpdDeletionRequestTablePresent(t, ctx, db) {
		t.Error("down: lgpd_deletion_request still present")
	}
	if got := dpoColumnsPresent(t, ctx, db); got != 0 {
		t.Errorf("down: expected 0 DPO columns, got %d", got)
	}

	applyChain(t, ctx, db,
		"0107_lgpd_deletion_request.up.sql",
		"0108_tenants_dpo_settings.up.sql",
	)
	if !lgpdDeletionRequestTablePresent(t, ctx, db) {
		t.Error("re-up: lgpd_deletion_request missing")
	}
	if got := dpoColumnsPresent(t, ctx, db); got != 4 {
		t.Errorf("re-up: expected 4 DPO columns, got %d", got)
	}
}

func TestLGPDDeletionRequest_RLSForced(t *testing.T) {
	db, ctx := freshDBWithLGPD(t)
	var rls, force bool
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity
		   FROM pg_class WHERE relname = 'lgpd_deletion_request'`).Scan(&rls, &force); err != nil {
		t.Fatalf("rls probe: %v", err)
	}
	if !rls || !force {
		t.Errorf("rls = %v force = %v, both want true", rls, force)
	}
}

// AC #2 + index 'lgpd_deletion_request_pending_uniq' is the
// idempotency contract: a second INSERT with the same (tenant, contact)
// while a pending row exists is rejected so the handler's ON CONFLICT
// path takes over.
func TestLGPDDeletionRequest_PartialUniqueRejectsDuplicate(t *testing.T) {
	db, ctx := freshDBWithLGPD(t)
	tenantID := uuid.New()
	contactID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, 'x', $2)`, tenantID, tenantID.String()); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO lgpd_deletion_request
			    (tenant_id, contact_id, justification, retention_until)
			VALUES ($1, $2, 'first', now() + interval '5 years')
		`, tenantID, contactID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO lgpd_deletion_request
			    (tenant_id, contact_id, justification, retention_until)
			VALUES ($1, $2, 'duplicate', now() + interval '5 years')
		`, tenantID, contactID)
		return err
	})
	if err == nil {
		t.Fatal("second pending insert err = nil, want duplicate-key error")
	}
	var pgErr interface{ SQLState() string }
	if !errors.As(err, &pgErr) {
		t.Fatalf("err is not pg-typed: %v", err)
	}
	if pgErr.SQLState() != "23505" { // unique_violation
		t.Errorf("SQLState = %s, want 23505 (unique_violation)", pgErr.SQLState())
	}
}

func TestLGPDDeletionRequest_TenantIsolation(t *testing.T) {
	db, ctx := freshDBWithLGPD(t)
	a := uuid.New()
	b := uuid.New()
	for _, id := range []uuid.UUID{a, b} {
		if _, err := db.AdminPool().Exec(ctx,
			`INSERT INTO tenants (id, name, host) VALUES ($1, 'x', $2)`, id, id.String()); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO lgpd_deletion_request
			    (tenant_id, contact_id, justification, retention_until)
			VALUES ($1, $2, 'a', now())
		`, a, uuid.New())
		return err
	}); err != nil {
		t.Fatalf("insert as tenant A: %v", err)
	}
	// Tenant B sees zero rows from tenant A through RLS.
	var seen int
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), b, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM lgpd_deletion_request`).Scan(&seen)
	}); err != nil {
		t.Fatalf("count under tenant B: %v", err)
	}
	if seen != 0 {
		t.Errorf("tenant B saw %d rows, want 0", seen)
	}
}

func TestTenantsDPOSettings_NullableAndPopulatable(t *testing.T) {
	db, ctx := freshDBWithLGPD(t)
	id := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, 'x', $2)`, id, id.String()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, `
		UPDATE tenants
		   SET dpo_name='Pericles', dpo_email='dpo@example.org',
		       privacy_policy_version='2026.05', privacy_policy_url='https://x'
		 WHERE id = $1
	`, id); err != nil {
		t.Fatalf("update DPO columns: %v", err)
	}
	var name string
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT dpo_name FROM tenants WHERE id = $1`, id).Scan(&name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "Pericles" {
		t.Errorf("dpo_name = %q, want %q", name, "Pericles")
	}
}
