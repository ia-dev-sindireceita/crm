package postgres_test

// SIN-63945 / UX-F8 — migration 0118 regression test.
//
// SE residual risk R4 requires the backfill to preserve the effective
// Yellow-field set:
//
//   * existing opt_in = true rows must end up with
//     structured_fields = {email, phone, cnpj}
//   * existing opt_in = false rows must end up with
//     structured_fields = '{}'
//
// Lives in the parent postgres_test package (not the
// internal/adapter/db/postgres/aipolicy subpackage) to share the
// TestMain + harness with the other postgres_test files — tests that
// need testpg in a separate binary race the ALTER ROLE bootstrap on
// the shared CI cluster (SQLSTATE 28P01), per the SIN-62351 W1A test.

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

// freshDBWithAIPolicyStructuredFields applies the chain up to 0118
// (the structured_fields column migration). The chain reuses every
// 0098 prerequisite + the intermediate migrations so the column
// addition lands on the production-shaped schema.
func freshDBWithAIPolicyStructuredFields(t *testing.T) (*testpg.DB, context.Context) {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0098_ai_policy_ai_summary_product_argument.up.sql",
		"0118_ai_policy_structured_fields.up.sql",
	)
	return db, ctx
}

// TestMigration0118_StructuredFieldsBackfill_OptInTrue pins SE R4:
// a tenant that had opt_in = true BEFORE the migration ends up with
// structured_fields = {email, phone, cnpj} after the migration runs,
// so its effective Yellow-field set does not change at deploy time.
func TestMigration0118_StructuredFieldsBackfill_OptInTrue(t *testing.T) {
	t.Parallel()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0098_ai_policy_ai_summary_product_argument.up.sql",
	)
	tenantID := seedAIPolicyTenant(t, db.AdminPool())
	// Insert via admin pool to bypass RLS; opt_in = true is the legacy
	// state the migration must backfill from.
	if _, err := db.AdminPool().Exec(ctx, `
		INSERT INTO ai_policy
		  (tenant_id, scope_type, scope_id,
		   model, prompt_version, tone, language,
		   ai_enabled, anonymize, opt_in)
		VALUES ($1, 'tenant', $2, 'gemini-flash', 'v1', 'neutro', 'pt-BR', true, true, true)
	`, tenantID, tenantID.String()); err != nil {
		t.Fatalf("seed pre-migration ai_policy: %v", err)
	}

	applyChain(t, ctx, db, "0118_ai_policy_structured_fields.up.sql")

	gotFields := readStructuredFields(t, ctx, db, tenantID)
	want := []string{"cnpj", "email", "phone"}
	if !equalStrSets(gotFields, want) {
		t.Fatalf("opt_in=true backfill = %v, want %v", gotFields, want)
	}
}

// TestMigration0118_StructuredFieldsBackfill_OptInFalse pins the other
// side of SE R4: opt_in = false rows end up with the empty default.
func TestMigration0118_StructuredFieldsBackfill_OptInFalse(t *testing.T) {
	t.Parallel()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0098_ai_policy_ai_summary_product_argument.up.sql",
	)
	tenantID := seedAIPolicyTenant(t, db.AdminPool())
	if _, err := db.AdminPool().Exec(ctx, `
		INSERT INTO ai_policy
		  (tenant_id, scope_type, scope_id,
		   model, prompt_version, tone, language,
		   ai_enabled, anonymize, opt_in)
		VALUES ($1, 'tenant', $2, 'gemini-flash', 'v1', 'neutro', 'pt-BR', true, true, false)
	`, tenantID, tenantID.String()); err != nil {
		t.Fatalf("seed pre-migration ai_policy: %v", err)
	}

	applyChain(t, ctx, db, "0118_ai_policy_structured_fields.up.sql")

	gotFields := readStructuredFields(t, ctx, db, tenantID)
	if len(gotFields) != 0 {
		t.Fatalf("opt_in=false backfill = %v, want []", gotFields)
	}
}

// TestMigration0118_DefaultsToEmptyOnNewRows confirms the column
// DEFAULT '{}' applies for rows inserted AFTER the migration runs.
func TestMigration0118_DefaultsToEmptyOnNewRows(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithAIPolicyStructuredFields(t)
	tenantID := seedAIPolicyTenant(t, db.AdminPool())
	if _, err := db.AdminPool().Exec(ctx, `
		INSERT INTO ai_policy
		  (tenant_id, scope_type, scope_id,
		   model, prompt_version, tone, language,
		   ai_enabled, anonymize, opt_in)
		VALUES ($1, 'tenant', $2, 'gemini-flash', 'v1', 'neutro', 'pt-BR', true, true, false)
	`, tenantID, tenantID.String()); err != nil {
		t.Fatalf("post-migration insert: %v", err)
	}
	gotFields := readStructuredFields(t, ctx, db, tenantID)
	if len(gotFields) != 0 {
		t.Fatalf("default = %v, want []", gotFields)
	}
}

// TestMigration0118_DownDropsColumn confirms the rollback path drops
// the column cleanly. The "down" leaves opt_in as the single source
// of truth (matching pre-F8 behaviour). Down + re-up MUST be a no-op.
func TestMigration0118_DownDropsColumn(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithAIPolicyStructuredFields(t)

	// Apply down — column gone.
	applyChain(t, ctx, db, "0118_ai_policy_structured_fields.down.sql")

	var present int
	if err := db.SuperuserPool().QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'ai_policy' AND column_name = 'structured_fields'
	`).Scan(&present); err != nil {
		t.Fatalf("column-exists probe: %v", err)
	}
	if present != 0 {
		t.Fatalf("down migration left structured_fields column in place (count=%d)", present)
	}

	// Re-apply up to make sure idempotency holds (IF NOT EXISTS).
	applyChain(t, ctx, db, "0118_ai_policy_structured_fields.up.sql")
	if err := db.SuperuserPool().QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'ai_policy' AND column_name = 'structured_fields'
	`).Scan(&present); err != nil {
		t.Fatalf("post-reup probe: %v", err)
	}
	if present != 1 {
		t.Fatalf("re-applied up migration did not restore column (count=%d)", present)
	}
}

func readStructuredFields(t *testing.T, ctx context.Context, db *testpg.DB, tenantID uuid.UUID) []string {
	t.Helper()
	var fields []string
	if err := db.SuperuserPool().QueryRow(ctx, `
		SELECT structured_fields FROM ai_policy WHERE tenant_id = $1
	`, tenantID).Scan(&fields); err != nil {
		t.Fatalf("read structured_fields: %v", err)
	}
	return fields
}

func equalStrSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	la := append([]string(nil), a...)
	lb := append([]string(nil), b...)
	sort.Strings(la)
	sort.Strings(lb)
	for i := range la {
		if la[i] != lb[i] {
			return false
		}
	}
	return true
}
