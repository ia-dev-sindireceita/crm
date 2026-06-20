package postgres_test

// SIN-65363 — migration 0123 regression test.
//
// The LGPD consent gate becomes opt-in by config. Migration 0123 adds
// ai_policy.consent_required BOOLEAN NOT NULL DEFAULT false. The
// additive DEFAULT false is the backward-compatibility contract: every
// row that existed BEFORE the migration, and every row inserted AFTER
// without setting the column, must read back false (gate OFF) so no
// tenant is surprised by a consent modal at deploy time.
//
// Lives in the parent postgres_test package (not the
// internal/adapter/db/postgres/aipolicy subpackage) to share the
// TestMain + harness with the other postgres_test files — tests that
// need testpg in a separate binary race the ALTER ROLE bootstrap on
// the shared CI cluster (SQLSTATE 28P01), per the SIN-62351 W1A test.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

// freshDBWithAIPolicyConsentRequired applies the chain up to 0123 (the
// consent_required column migration), reusing the 0098 + 0118
// prerequisites so the column addition lands on the production-shaped
// schema.
func freshDBWithAIPolicyConsentRequired(t *testing.T) (*testpg.DB, context.Context) {
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
		"0123_ai_policy_consent_required.up.sql",
	)
	return db, ctx
}

// TestMigration0123_ExistingRowGetsFalse pins the backward-compat
// contract: a row that existed BEFORE the migration ends up with
// consent_required = false (gate OFF) — no tenant gains a consent modal
// silently at deploy time.
func TestMigration0123_ExistingRowGetsFalse(t *testing.T) {
	t.Parallel()
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
	tenantID := seedAIPolicyTenant(t, db.AdminPool())
	if _, err := db.AdminPool().Exec(ctx, `
		INSERT INTO ai_policy
		  (tenant_id, scope_type, scope_id,
		   model, prompt_version, tone, language,
		   ai_enabled, anonymize, opt_in)
		VALUES ($1, 'tenant', $2, 'gemini-flash', 'v1', 'neutro', 'pt-BR', true, true, true)
	`, tenantID, tenantID.String()); err != nil {
		t.Fatalf("seed pre-migration ai_policy: %v", err)
	}

	applyChain(t, ctx, db, "0123_ai_policy_consent_required.up.sql")

	if got := readConsentRequired(t, ctx, db, tenantID); got {
		t.Fatalf("pre-existing row consent_required = true after migration; want false (off by default)")
	}
}

// TestMigration0123_DefaultsToFalseOnNewRows confirms the column
// DEFAULT false applies for rows inserted AFTER the migration runs.
func TestMigration0123_DefaultsToFalseOnNewRows(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithAIPolicyConsentRequired(t)
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
	if got := readConsentRequired(t, ctx, db, tenantID); got {
		t.Fatalf("new row consent_required = true; want false (column default)")
	}
}

// TestMigration0123_DownDropsColumn confirms the rollback drops the
// column cleanly, and down + re-up is a no-op (IF NOT EXISTS).
func TestMigration0123_DownDropsColumn(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithAIPolicyConsentRequired(t)

	applyChain(t, ctx, db, "0123_ai_policy_consent_required.down.sql")

	var present int
	if err := db.SuperuserPool().QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'ai_policy' AND column_name = 'consent_required'
	`).Scan(&present); err != nil {
		t.Fatalf("column-exists probe: %v", err)
	}
	if present != 0 {
		t.Fatalf("down migration left consent_required column in place (count=%d)", present)
	}

	applyChain(t, ctx, db, "0123_ai_policy_consent_required.up.sql")
	if err := db.SuperuserPool().QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'ai_policy' AND column_name = 'consent_required'
	`).Scan(&present); err != nil {
		t.Fatalf("post-reup probe: %v", err)
	}
	if present != 1 {
		t.Fatalf("re-applied up migration did not restore column (count=%d)", present)
	}
}

func readConsentRequired(t *testing.T, ctx context.Context, db *testpg.DB, tenantID uuid.UUID) bool {
	t.Helper()
	var consent bool
	if err := db.SuperuserPool().QueryRow(ctx, `
		SELECT consent_required FROM ai_policy WHERE tenant_id = $1
	`, tenantID).Scan(&consent); err != nil {
		t.Fatalf("read consent_required: %v", err)
	}
	return consent
}
