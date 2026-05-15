package postgres_test

// SIN-62787 (Fase 2 F2-04): 0093_funnel_stage_transition.
// Applies 0004 → 0005 → 0088 → 0092 → 0093 on top of the harness default
// 0001-0003 chain. Covers up/down round-trip, auto-seed on tenant insert,
// backfill of existing tenants, idempotency of the seed function,
// indexes, FK constraints, and RLS posture.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

var funnelF2TableNames = []string{
	"funnel_stage",
	"funnel_transition",
}

var funnelF2DefaultKeys = []string{
	"novo", "qualificando", "proposta", "ganho", "perdido",
}

// freshDBWithFunnelF2 applies the full chain up to 0093 to a fresh DB.
func freshDBWithFunnelF2(t *testing.T) (*testpg.DB, context.Context) {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0092_identity_link_assignment_history.up.sql",
		"0093_funnel_stage_transition.up.sql",
	)
	return db, ctx
}

func funnelF2TablesPresent(t *testing.T, ctx context.Context, db *testpg.DB) int {
	t.Helper()
	var count int
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relname = ANY($1) AND n.nspname = 'public'`,
		funnelF2TableNames).Scan(&count); err != nil {
		t.Fatalf("table-exists probe: %v", err)
	}
	return count
}

// TestFunnelF2_UpDownUp round-trips 0093 — up, down, up, down twice —
// asserting the two tables come and go cleanly and that down is
// idempotent.
func TestFunnelF2_UpDownUp(t *testing.T) {
	db, ctx := freshDBWithFunnelF2(t)

	if got := funnelF2TablesPresent(t, ctx, db); got != len(funnelF2TableNames) {
		t.Fatalf("after initial up: got %d/%d tables", got, len(funnelF2TableNames))
	}
	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0093_funnel_stage_transition.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0093_funnel_stage_transition.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if got := funnelF2TablesPresent(t, ctx, db); got != 0 {
		t.Fatalf("after down: %d tables still present", got)
	}
	// The tenants-seed trigger must also be gone (down is the only way to
	// fully undo the migration's effect on the tenants table).
	var trigCount int
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_trigger
		  WHERE tgname = 'tenants_seed_funnel_stages'`).Scan(&trigCount); err != nil {
		t.Fatalf("trigger probe: %v", err)
	}
	if trigCount != 0 {
		t.Errorf("tenants_seed_funnel_stages still present after down")
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if got := funnelF2TablesPresent(t, ctx, db); got != len(funnelF2TableNames) {
		t.Fatalf("after re-up: got %d tables", got)
	}
	// Down idempotency.
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down #2: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down #3: %v", err)
	}
}

// stageKeysFor reads the funnel_stage keys for a tenant, sorted by
// position. Tests use it to confirm seeded ordering.
func stageKeysFor(t *testing.T, ctx context.Context, db *testpg.DB, tenantID uuid.UUID) []string {
	t.Helper()
	rows, err := db.AdminPool().Query(ctx,
		`SELECT key FROM funnel_stage WHERE tenant_id = $1 ORDER BY position ASC`,
		tenantID)
	if err != nil {
		t.Fatalf("query stages: %v", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan key: %v", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	return keys
}

// TestFunnelF2_AutoSeedOnTenantInsert proves the AFTER INSERT trigger on
// tenants seeds the five default stages with the documented positions
// and is_default=true.
func TestFunnelF2_AutoSeedOnTenantInsert(t *testing.T) {
	db, ctx := freshDBWithFunnelF2(t)

	tenantID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantID, "auto-seed", fmt.Sprintf("auto-%s.crm.local", tenantID)); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	keys := stageKeysFor(t, ctx, db, tenantID)
	if got, want := keys, funnelF2DefaultKeys; !equalSlice(got, want) {
		t.Errorf("seeded keys = %v, want %v", got, want)
	}

	// Verify positions and is_default for every seeded row.
	type row struct {
		key       string
		label     string
		position  int
		isDefault bool
	}
	rows, err := db.AdminPool().Query(ctx,
		`SELECT key, label, position, is_default FROM funnel_stage
		  WHERE tenant_id = $1 ORDER BY position ASC`, tenantID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.key, &r.label, &r.position, &r.isDefault); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	want := []row{
		{"novo", "Novo", 1, true},
		{"qualificando", "Qualificando", 2, true},
		{"proposta", "Proposta", 3, true},
		{"ganho", "Ganho", 4, true},
		{"perdido", "Perdido", 5, true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestFunnelF2_BackfillSeedsExistingTenants seeds two tenants BEFORE
// applying 0093, then verifies both received the five default stages.
// Re-applying the migration MUST NOT create duplicate rows.
func TestFunnelF2_BackfillSeedsExistingTenants(t *testing.T) {
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0092_identity_link_assignment_history.up.sql",
	)

	tenantA := uuid.New()
	tenantB := uuid.New()
	for _, p := range []struct {
		id   uuid.UUID
		name string
	}{{tenantA, "A"}, {tenantB, "B"}} {
		if _, err := db.AdminPool().Exec(ctx,
			`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
			p.id, p.name, fmt.Sprintf("%s-%s.crm.local", strings.ToLower(p.name), p.id)); err != nil {
			t.Fatalf("seed tenant %s: %v", p.name, err)
		}
	}

	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0093_funnel_stage_transition.up.sql"))
	if err != nil {
		t.Fatalf("read 0093: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("apply 0093: %v", err)
	}

	for _, tid := range []uuid.UUID{tenantA, tenantB} {
		got := stageKeysFor(t, ctx, db, tid)
		if !equalSlice(got, funnelF2DefaultKeys) {
			t.Errorf("tenant %s: backfilled keys = %v, want %v", tid, got, funnelF2DefaultKeys)
		}
	}

	// Idempotency: re-applying the migration must not seed duplicates.
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply 0093: %v", err)
	}
	for _, tid := range []uuid.UUID{tenantA, tenantB} {
		var n int
		if err := db.AdminPool().QueryRow(ctx,
			`SELECT count(*) FROM funnel_stage WHERE tenant_id = $1`, tid).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 5 {
			t.Errorf("tenant %s: after re-apply got %d stages, want 5", tid, n)
		}
	}
}

// TestFunnelF2_StageUniqueOnTenantKey enforces UNIQUE (tenant_id, key) so
// two stages with the same key cannot coexist on the same tenant. A
// distinct tenant with the same key remains legal (per-tenant funnels).
func TestFunnelF2_StageUniqueOnTenantKey(t *testing.T) {
	db, ctx := freshDBWithFunnelF2(t)

	tenantA, _ := seedTenantUserMaster(t, db)
	// tenantA already has the five default stages from the trigger; a
	// second 'novo' on tenantA must violate the constraint.
	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO funnel_stage (tenant_id, key, label, position) VALUES ($1, 'novo', 'Dup', 99)`,
		tenantA)
	if err == nil {
		t.Fatal("expected UNIQUE (tenant_id, key) violation, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}

	// Same key on a different tenant is legal.
	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "B", fmt.Sprintf("b-%s.crm.local", tenantB)); err != nil {
		t.Fatalf("seed tenantB: %v", err)
	}
	// tenantB now has its own 'novo' from auto-seed; verify it.
	got := stageKeysFor(t, ctx, db, tenantB)
	if !equalSlice(got, funnelF2DefaultKeys) {
		t.Errorf("tenantB: got %v, want %v", got, funnelF2DefaultKeys)
	}
}

// TestFunnelF2_TransitionFKShapes confirms:
//   - from_stage_id may be NULL (first entry into the funnel)
//   - to_stage_id is NOT NULL
//   - to_stage_id with ON DELETE RESTRICT blocks deletion of a referenced stage.
func TestFunnelF2_TransitionFKShapes(t *testing.T) {
	db, ctx := freshDBWithFunnelF2(t)
	tenantA, userID := seedTenantUserMaster(t, db)

	contact := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, 'X')`,
		contact, tenantA); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	conv := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO conversation (id, tenant_id, contact_id, channel) VALUES ($1, $2, $3, 'whatsapp')`,
		conv, tenantA, contact); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	var novoID uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT id FROM funnel_stage WHERE tenant_id=$1 AND key='novo'`, tenantA).Scan(&novoID); err != nil {
		t.Fatalf("read novo: %v", err)
	}

	t.Run("from_stage_null_accepted", func(t *testing.T) {
		_, err := db.AdminPool().Exec(ctx,
			`INSERT INTO funnel_transition (tenant_id, conversation_id, from_stage_id, to_stage_id, transitioned_by_user_id)
			 VALUES ($1, $2, NULL, $3, $4)`,
			tenantA, conv, novoID, userID)
		if err != nil {
			t.Errorf("expected NULL from_stage_id accepted, got: %v", err)
		}
	})

	t.Run("to_stage_null_rejected", func(t *testing.T) {
		_, err := db.AdminPool().Exec(ctx,
			`INSERT INTO funnel_transition (tenant_id, conversation_id, from_stage_id, to_stage_id, transitioned_by_user_id)
			 VALUES ($1, $2, $3, NULL, $4)`,
			tenantA, conv, novoID, userID)
		if err == nil {
			t.Fatal("expected NOT NULL violation, got nil")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "null value in column") {
			t.Errorf("expected NOT NULL error, got: %v", err)
		}
	})

	t.Run("delete_referenced_stage_restricted", func(t *testing.T) {
		_, err := db.AdminPool().Exec(ctx,
			`DELETE FROM funnel_stage WHERE id = $1`, novoID)
		if err == nil {
			t.Fatal("expected RESTRICT violation, got nil")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "foreign key constraint") {
			t.Errorf("expected FK violation, got: %v", err)
		}
	})
}

// TestFunnelF2_IndexesPresent asserts the two indexes named in the
// SIN-62787 AC exist on the right columns with the right ordering.
func TestFunnelF2_IndexesPresent(t *testing.T) {
	db, ctx := freshDBWithFunnelF2(t)
	want := map[string]string{
		"funnel_stage_tenant_position_idx":     "funnel_stage",
		"funnel_transition_tenant_conv_at_idx": "funnel_transition",
	}
	for idx, tbl := range want {
		var def string
		if err := db.SuperuserPool().QueryRow(ctx,
			`SELECT indexdef FROM pg_indexes WHERE indexname = $1 AND tablename = $2`,
			idx, tbl).Scan(&def); err != nil {
			t.Errorf("missing index %s on %s: %v", idx, tbl, err)
			continue
		}
		if !strings.Contains(def, "tenant_id") {
			t.Errorf("index %s does not include tenant_id: %s", idx, def)
		}
	}
	// transition index must be (tenant_id, conversation_id, transitioned_at DESC).
	var def string
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'funnel_transition_tenant_conv_at_idx'`).Scan(&def); err != nil {
		t.Fatalf("read transition index def: %v", err)
	}
	if !strings.Contains(def, "transitioned_at DESC") {
		t.Errorf("transition index missing transitioned_at DESC: %s", def)
	}
}

// TestFunnelF2_RLS covers the four canonical regressions from ADR
// 0072: zero-rows without WithTenant, isolation between tenants, WITH
// CHECK on cross-tenant INSERT, and FORCE ROW LEVEL SECURITY on owner.
func TestFunnelF2_RLS(t *testing.T) {
	db, ctx := freshDBWithFunnelF2(t)
	tenantA, userID := seedTenantUserMaster(t, db)
	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "tenantB", fmt.Sprintf("b-%s.crm.local", tenantB)); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	// Seed one funnel_transition per tenant via app_admin so we have rows
	// to look at under RLS.
	contactA, contactB := uuid.New(), uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, 'A'), ($3, $4, 'B')`,
		contactA, tenantA, contactB, tenantB); err != nil {
		t.Fatalf("seed contacts: %v", err)
	}
	convA := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO conversation (id, tenant_id, contact_id, channel) VALUES ($1, $2, $3, 'whatsapp')`,
		convA, tenantA, contactA); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	var novoA uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT id FROM funnel_stage WHERE tenant_id=$1 AND key='novo'`, tenantA).Scan(&novoA); err != nil {
		t.Fatalf("read novoA: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO funnel_transition (tenant_id, conversation_id, to_stage_id, transitioned_by_user_id)
		 VALUES ($1, $2, $3, $4)`,
		tenantA, convA, novoA, userID); err != nil {
		t.Fatalf("seed transition: %v", err)
	}

	t.Run("no_tenant_set_returns_zero", func(t *testing.T) {
		for _, table := range funnelF2TableNames {
			var n int
			if err := db.RuntimePool().QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&n); err != nil {
				t.Errorf("count %s: %v", table, err)
				continue
			}
			if n != 0 {
				t.Errorf("runtime sees %d %s without WithTenant; want 0", n, table)
			}
		}
	})

	t.Run("tenant_isolation_on_stage", func(t *testing.T) {
		// Auto-seed gave both tenants their 5 stages. WithTenant(A) must
		// only see A's rows.
		if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
			var leak int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM funnel_stage WHERE tenant_id = $1`, tenantB).Scan(&leak); err != nil {
				return err
			}
			if leak != 0 {
				t.Errorf("tenant A leaked B by literal: %d, want 0", leak)
			}
			var own int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM funnel_stage`).Scan(&own); err != nil {
				return err
			}
			if own != 5 {
				t.Errorf("tenant A sees %d own stages, want 5", own)
			}
			return nil
		}); err != nil {
			t.Fatalf("WithTenant(A): %v", err)
		}
	})

	t.Run("insert_wrong_tenant_fails", func(t *testing.T) {
		err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
			_, e := tx.Exec(ctx,
				`INSERT INTO funnel_stage (tenant_id, key, label, position) VALUES ($1, 'bogus', 'X', 99)`,
				tenantB)
			return e
		})
		if err == nil || !strings.Contains(err.Error(), "row-level security") {
			t.Errorf("expected row-level-security violation, got: %v", err)
		}
	})

	t.Run("force_rls_on_owner", func(t *testing.T) {
		for _, table := range funnelF2TableNames {
			var force bool
			if err := db.SuperuserPool().QueryRow(ctx,
				`SELECT relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&force); err != nil {
				t.Errorf("read relforcerowsecurity(%s): %v", table, err)
				continue
			}
			if !force {
				t.Errorf("%s: FORCE ROW LEVEL SECURITY = false (ADR 0072 violation)", table)
			}
		}
	})
}

// equalSlice reports whether two string slices have identical elements in
// the same order.
func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
