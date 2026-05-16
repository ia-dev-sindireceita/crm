package postgres_test

// SIN-62900 / Fase 3 W1A acceptance for
// 0098_ai_policy_ai_summary_product_argument:
//
//   #1 up/down/up idempotent on the shared CI cluster, with the four tables
//      coming and going cleanly
//   #2 RLS isolates by tenant on every new table (tenant B cannot read A's
//      rows under WithTenant(B))
//   #3 ai_policy UNIQUE(tenant_id, scope_type, scope_id) rejects duplicate
//      scope rows; scope_type CHECK rejects unknown values
//   #4 product_argument FK product_id CASCADEs on product delete; UNIQUE
//      (tenant_id, product_id, scope_type, scope_id) rejects duplicates
//   #5 ai_summary FK conversation_id CASCADEs on conversation delete
//
// Tests live in postgres_test (same package as other migration tests) so
// they share the cluster-bootstrap state and don't re-fight the SQLSTATE
// 28P01 regression pattern (SIN-62726 / SIN-62750).

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

// aiW1ATableNames lists every table created by 0098.
var aiW1ATableNames = []string{
	"ai_policy",
	"ai_summary",
	"product",
	"product_argument",
}

// freshDBWithAIW1A applies the full chain up to 0098 to a fresh DB. The
// chain is the minimum needed to satisfy 0098's FKs:
//   - 0004 tenants
//   - 0005 users (conversation's assigned_user_id REFERENCES users — even
//     though we never set it, the FK must resolve at table creation time)
//   - 0088 inbox_contacts (creates the conversation table — ai_summary
//     FK target)
//   - 0098 — the migration under test
func freshDBWithAIW1A(t *testing.T) (*testpg.DB, context.Context) {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0098_ai_policy_ai_summary_product_argument.up.sql",
	)
	return db, ctx
}

func aiW1ATablesPresent(t *testing.T, ctx context.Context, db *testpg.DB) int {
	t.Helper()
	var count int
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relname = ANY($1) AND n.nspname = 'public'`,
		aiW1ATableNames).Scan(&count); err != nil {
		t.Fatalf("table-exists probe: %v", err)
	}
	return count
}

// seedTenantB inserts a second tenant for the RLS isolation tests. The
// other-tenant tests need a tenant that does NOT show up in
// seedTenantUserMaster's seed.
func seedTenantB(t *testing.T, ctx context.Context, db *testpg.DB) uuid.UUID {
	t.Helper()
	tid := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tid, "tenantB", fmt.Sprintf("b-%s.crm.local", tid)); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}
	return tid
}

// seedConversation inserts a contact + conversation under app_admin
// (BYPASSRLS=true) and returns the conversation id. Used to satisfy the
// ai_summary FK in tests that probe summary lifecycle.
func seedConversation(t *testing.T, ctx context.Context, db *testpg.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	var contactID uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`INSERT INTO contact (tenant_id, display_name) VALUES ($1, $2) RETURNING id`,
		tenantID, "fixture").Scan(&contactID); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	var convID uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`INSERT INTO conversation (tenant_id, contact_id, channel, state)
		 VALUES ($1, $2, 'whatsapp', 'open') RETURNING id`,
		tenantID, contactID).Scan(&convID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return convID
}

// ---------------------------------------------------------------------------
// AC #1 — up/down idempotency
// ---------------------------------------------------------------------------

// TestAIW1AMigration_UpDownUp proves both directions of 0098 are
// idempotent and round-trip safe.
func TestAIW1AMigration_UpDownUp(t *testing.T) {
	db, ctx := freshDBWithAIW1A(t)

	if got := aiW1ATablesPresent(t, ctx, db); got != len(aiW1ATableNames) {
		t.Fatalf("after initial up: got %d/%d tables", got, len(aiW1ATableNames))
	}

	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(),
		"0098_ai_policy_ai_summary_product_argument.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(),
		"0098_ai_policy_ai_summary_product_argument.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}

	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if got := aiW1ATablesPresent(t, ctx, db); got != 0 {
		t.Fatalf("after down: %d/%d tables still present", got, len(aiW1ATableNames))
	}

	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if got := aiW1ATablesPresent(t, ctx, db); got != len(aiW1ATableNames) {
		t.Fatalf("after re-up: got %d/%d tables", got, len(aiW1ATableNames))
	}

	// Down-twice and up-twice must both be no-ops without error.
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down (idempotent): %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("apply up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("apply up (idempotent): %v", err)
	}
}

// TestAIW1AForceRLS_AppliesToOwner: relforcerowsecurity=true on every
// new table. Canary against any future migration that forgets FORCE.
func TestAIW1AForceRLS_AppliesToOwner(t *testing.T) {
	db, ctx := freshDBWithAIW1A(t)
	for _, table := range aiW1ATableNames {
		var force bool
		row := db.SuperuserPool().QueryRow(ctx,
			`SELECT relforcerowsecurity FROM pg_class WHERE relname = $1`, table)
		if err := row.Scan(&force); err != nil {
			t.Fatalf("read relforcerowsecurity(%s): %v", table, err)
		}
		if !force {
			t.Errorf("table %s: FORCE ROW LEVEL SECURITY = false (ADR-0072 violation)", table)
		}
	}
}

// ---------------------------------------------------------------------------
// AC #2 — RLS isolates by tenant on every new table
// ---------------------------------------------------------------------------

// TestAIW1ARLS_TenantIsolation_AllTables seeds one row per tenant on each
// of the four tables and confirms runtime under WithTenant(B) sees zero
// of tenant A's rows. The single test sweeps all four tables so a
// regression on any one of them is caught.
func TestAIW1ARLS_TenantIsolation_AllTables(t *testing.T) {
	db, ctx := freshDBWithAIW1A(t)

	tenantA, _ := seedTenantUserMaster(t, db)
	tenantB := seedTenantB(t, ctx, db)

	convA := seedConversation(t, ctx, db, tenantA)
	convB := seedConversation(t, ctx, db, tenantB)

	// Seed one row in each table for both tenants via AdminPool
	// (BYPASSRLS=true).
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_policy (tenant_id, scope_type, scope_id, ai_enabled, anonymize, opt_in)
		 VALUES ($1, 'tenant', $2, true, true, true),
		        ($3, 'tenant', $4, true, true, true)`,
		tenantA, tenantA.String(), tenantB, tenantB.String()); err != nil {
		t.Fatalf("seed ai_policy: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_summary (tenant_id, conversation_id, summary_text, model, tokens_in, tokens_out)
		 VALUES ($1, $2, 'A-summary', 'openrouter/auto', 10, 20),
		        ($3, $4, 'B-summary', 'openrouter/auto', 10, 20)`,
		tenantA, convA, tenantB, convB); err != nil {
		t.Fatalf("seed ai_summary: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO product (tenant_id, name, price_cents, tags)
		 VALUES ($1, 'Product A', 100, ARRAY['promo']),
		        ($2, 'Product B', 200, ARRAY['promo'])`,
		tenantA, tenantB); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	var prodA, prodB uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT id FROM product WHERE tenant_id = $1`, tenantA).Scan(&prodA); err != nil {
		t.Fatalf("read product A id: %v", err)
	}
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT id FROM product WHERE tenant_id = $1`, tenantB).Scan(&prodB); err != nil {
		t.Fatalf("read product B id: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO product_argument (tenant_id, product_id, scope_type, scope_id, argument_text)
		 VALUES ($1, $2, 'tenant', $3, 'argument A'),
		        ($4, $5, 'tenant', $6, 'argument B')`,
		tenantA, prodA, tenantA.String(), tenantB, prodB, tenantB.String()); err != nil {
		t.Fatalf("seed product_argument: %v", err)
	}

	// Under WithTenant(B), the runtime role must see exactly its own row
	// (count == 1) on each table — never A's.
	for _, table := range aiW1ATableNames {
		var seenTenants []uuid.UUID
		if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantB, func(tx pgx.Tx) error {
			q := fmt.Sprintf(`SELECT tenant_id FROM %s`, table)
			rows, err := tx.Query(ctx, q)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var tid uuid.UUID
				if err := rows.Scan(&tid); err != nil {
					return err
				}
				seenTenants = append(seenTenants, tid)
			}
			return rows.Err()
		}); err != nil {
			t.Fatalf("WithTenant(B) on %s: %v", table, err)
		}
		if len(seenTenants) != 1 || seenTenants[0] != tenantB {
			t.Errorf("table %s: WithTenant(B) saw %v, want [%s]", table, seenTenants, tenantB)
		}
	}
}

// TestAIW1ARLS_NoTenantSetReturnsZero: runtime pool without a WithTenant
// scope sees zero rows on every tenanted table (fail-closed; ADR-0072).
func TestAIW1ARLS_NoTenantSetReturnsZero(t *testing.T) {
	db, ctx := freshDBWithAIW1A(t)

	tenantA, _ := seedTenantUserMaster(t, db)
	convA := seedConversation(t, ctx, db, tenantA)

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_policy (tenant_id, scope_type, scope_id) VALUES ($1, 'tenant', $2)`,
		tenantA, tenantA.String()); err != nil {
		t.Fatalf("seed ai_policy: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_summary (tenant_id, conversation_id, summary_text, model, tokens_in, tokens_out)
		 VALUES ($1, $2, 'A', 'm', 1, 1)`, tenantA, convA); err != nil {
		t.Fatalf("seed ai_summary: %v", err)
	}
	var prodA uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`INSERT INTO product (tenant_id, name) VALUES ($1, 'P') RETURNING id`,
		tenantA).Scan(&prodA); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO product_argument (tenant_id, product_id, scope_type, scope_id, argument_text)
		 VALUES ($1, $2, 'tenant', $3, 'arg')`, tenantA, prodA, tenantA.String()); err != nil {
		t.Fatalf("seed product_argument: %v", err)
	}

	for _, table := range aiW1ATableNames {
		var n int
		q := fmt.Sprintf(`SELECT count(*) FROM %s`, table)
		if err := db.RuntimePool().QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("runtime pool with no GUC saw %d %s rows, want 0", n, table)
		}
	}
}

// ---------------------------------------------------------------------------
// AC #3 — ai_policy scope CHECK + UNIQUE constraints
// ---------------------------------------------------------------------------

// TestAIPolicy_ScopeTypeCheck rejects scope_type values outside the
// documented set.
func TestAIPolicy_ScopeTypeCheck(t *testing.T) {
	db, ctx := freshDBWithAIW1A(t)
	tenantA, _ := seedTenantUserMaster(t, db)

	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_policy (tenant_id, scope_type, scope_id) VALUES ($1, 'global', $2)`,
		tenantA, tenantA.String())
	if err == nil {
		t.Fatal("expected check-violation for scope_type='global', got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "ai_policy_scope_type_check") &&
		!strings.Contains(strings.ToLower(err.Error()), "check constraint") {
		t.Errorf("expected scope_type check-violation, got: %v", err)
	}
}

// TestAIPolicy_ScopeUniqueness: UNIQUE(tenant_id, scope_type, scope_id)
// rejects a second row with the same triple. Different scope_type or
// different scope_id is fine.
func TestAIPolicy_ScopeUniqueness(t *testing.T) {
	db, ctx := freshDBWithAIW1A(t)
	tenantA, _ := seedTenantUserMaster(t, db)

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_policy (tenant_id, scope_type, scope_id) VALUES ($1, 'tenant', $2)`,
		tenantA, tenantA.String()); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_policy (tenant_id, scope_type, scope_id) VALUES ($1, 'tenant', $2)`,
		tenantA, tenantA.String())
	if err == nil {
		t.Fatal("expected unique-violation on duplicate scope, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}

	// Same tenant, different scope_type → accepted.
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_policy (tenant_id, scope_type, scope_id) VALUES ($1, 'channel', 'whatsapp')`,
		tenantA); err != nil {
		t.Errorf("second scope (channel) rejected: %v", err)
	}
	// Same tenant + channel scope, different scope_id → accepted.
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_policy (tenant_id, scope_type, scope_id) VALUES ($1, 'channel', 'instagram')`,
		tenantA); err != nil {
		t.Errorf("third scope (channel/instagram) rejected: %v", err)
	}
}

// TestAIPolicy_OptInDefaultsOff confirms ai_enabled / opt_in default to
// false so a freshly-onboarded tenant starts with IA OFF (LGPD posture,
// ADR-0041). anonymize defaults to true. Regression check against any
// future migration that flips a default.
func TestAIPolicy_OptInDefaultsOff(t *testing.T) {
	db, ctx := freshDBWithAIW1A(t)
	tenantA, _ := seedTenantUserMaster(t, db)

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_policy (tenant_id, scope_type, scope_id) VALUES ($1, 'tenant', $2)`,
		tenantA, tenantA.String()); err != nil {
		t.Fatalf("seed ai_policy: %v", err)
	}

	var aiEnabled, anonymize, optIn bool
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT ai_enabled, anonymize, opt_in FROM ai_policy
		  WHERE tenant_id = $1 AND scope_type = 'tenant'`,
		tenantA).Scan(&aiEnabled, &anonymize, &optIn); err != nil {
		t.Fatalf("read ai_policy: %v", err)
	}
	if aiEnabled {
		t.Errorf("ai_enabled defaulted to true; want false (LGPD opt-in)")
	}
	if !anonymize {
		t.Errorf("anonymize defaulted to false; want true (ADR-0041 default-on)")
	}
	if optIn {
		t.Errorf("opt_in defaulted to true; want false (LGPD explicit consent)")
	}
}

// ---------------------------------------------------------------------------
// AC #4 — product_argument FK + UNIQUE
// ---------------------------------------------------------------------------

// TestProductArgument_CascadeOnProductDelete: deleting a product
// cascades to its arguments. Mirrors W2B's expectation that argument
// lifetime ≤ product lifetime.
func TestProductArgument_CascadeOnProductDelete(t *testing.T) {
	db, ctx := freshDBWithAIW1A(t)
	tenantA, _ := seedTenantUserMaster(t, db)

	var prodID uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`INSERT INTO product (tenant_id, name) VALUES ($1, 'Plan Pro') RETURNING id`,
		tenantA).Scan(&prodID); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO product_argument (tenant_id, product_id, scope_type, scope_id, argument_text)
		 VALUES ($1, $2, 'tenant', $3, 'pitch-tenant'),
		        ($1, $2, 'channel', 'whatsapp', 'pitch-whatsapp')`,
		tenantA, prodID, tenantA.String()); err != nil {
		t.Fatalf("seed product_argument: %v", err)
	}

	if _, err := db.AdminPool().Exec(ctx,
		`DELETE FROM product WHERE id = $1`, prodID); err != nil {
		t.Fatalf("delete product: %v", err)
	}

	var n int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM product_argument WHERE product_id = $1`, prodID).Scan(&n); err != nil {
		t.Fatalf("count arguments after delete: %v", err)
	}
	if n != 0 {
		t.Errorf("product delete left %d arguments; want 0", n)
	}
}

// TestProductArgument_Uniqueness: a duplicate (tenant, product, scope_type,
// scope_id) is rejected; a different scope_id under the same product is
// accepted.
func TestProductArgument_Uniqueness(t *testing.T) {
	db, ctx := freshDBWithAIW1A(t)
	tenantA, _ := seedTenantUserMaster(t, db)

	var prodID uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`INSERT INTO product (tenant_id, name) VALUES ($1, 'Plan Pro') RETURNING id`,
		tenantA).Scan(&prodID); err != nil {
		t.Fatalf("seed product: %v", err)
	}

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO product_argument (tenant_id, product_id, scope_type, scope_id, argument_text)
		 VALUES ($1, $2, 'channel', 'whatsapp', 'pitch-1')`,
		tenantA, prodID); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO product_argument (tenant_id, product_id, scope_type, scope_id, argument_text)
		 VALUES ($1, $2, 'channel', 'whatsapp', 'pitch-2')`,
		tenantA, prodID)
	if err == nil {
		t.Fatal("expected unique-violation on duplicate (product, scope), got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}

	// Same product + channel, different scope_id → accepted.
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO product_argument (tenant_id, product_id, scope_type, scope_id, argument_text)
		 VALUES ($1, $2, 'channel', 'instagram', 'pitch-3')`,
		tenantA, prodID); err != nil {
		t.Errorf("second scope rejected: %v", err)
	}
}

// TestProductArgument_ScopeTypeCheck rejects scope_type values outside
// the documented set on the argument table.
func TestProductArgument_ScopeTypeCheck(t *testing.T) {
	db, ctx := freshDBWithAIW1A(t)
	tenantA, _ := seedTenantUserMaster(t, db)

	var prodID uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`INSERT INTO product (tenant_id, name) VALUES ($1, 'P') RETURNING id`,
		tenantA).Scan(&prodID); err != nil {
		t.Fatalf("seed product: %v", err)
	}

	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO product_argument (tenant_id, product_id, scope_type, scope_id, argument_text)
		 VALUES ($1, $2, 'global', $3, 'arg')`,
		tenantA, prodID, tenantA.String())
	if err == nil {
		t.Fatal("expected check-violation for scope_type='global', got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "check constraint") &&
		!strings.Contains(strings.ToLower(err.Error()), "product_argument_scope_type_check") {
		t.Errorf("expected scope_type check-violation, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC #5 — ai_summary FK + tokens CHECK
// ---------------------------------------------------------------------------

// TestAISummary_CascadeOnConversationDelete: deleting a conversation
// cascades to its summaries (summary lifetime ≤ conversation lifetime).
func TestAISummary_CascadeOnConversationDelete(t *testing.T) {
	db, ctx := freshDBWithAIW1A(t)
	tenantA, _ := seedTenantUserMaster(t, db)
	convID := seedConversation(t, ctx, db, tenantA)

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_summary (tenant_id, conversation_id, summary_text, model, tokens_in, tokens_out)
		 VALUES ($1, $2, 'first', 'openrouter/auto', 50, 100),
		        ($1, $2, 'second', 'openrouter/auto', 60, 120)`,
		tenantA, convID); err != nil {
		t.Fatalf("seed ai_summary: %v", err)
	}

	if _, err := db.AdminPool().Exec(ctx,
		`DELETE FROM conversation WHERE id = $1`, convID); err != nil {
		t.Fatalf("delete conversation: %v", err)
	}

	var n int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM ai_summary WHERE conversation_id = $1`, convID).Scan(&n); err != nil {
		t.Fatalf("count summaries after delete: %v", err)
	}
	if n != 0 {
		t.Errorf("conversation delete left %d summaries; want 0", n)
	}
}

// TestAISummary_TokensNonNegative: tokens_in / tokens_out CHECK forbids
// negative counts (defence against a buggy estimator).
func TestAISummary_TokensNonNegative(t *testing.T) {
	db, ctx := freshDBWithAIW1A(t)
	tenantA, _ := seedTenantUserMaster(t, db)
	convID := seedConversation(t, ctx, db, tenantA)

	for _, col := range []string{"tokens_in", "tokens_out"} {
		col := col
		t.Run(col+"_negative_rejected", func(t *testing.T) {
			var stmt string
			switch col {
			case "tokens_in":
				stmt = `INSERT INTO ai_summary
				          (tenant_id, conversation_id, summary_text, model, tokens_in, tokens_out)
				        VALUES ($1, $2, 'x', 'm', -1, 0)`
			default:
				stmt = `INSERT INTO ai_summary
				          (tenant_id, conversation_id, summary_text, model, tokens_in, tokens_out)
				        VALUES ($1, $2, 'x', 'm', 0, -1)`
			}
			_, err := db.AdminPool().Exec(ctx, stmt, tenantA, convID)
			if err == nil {
				t.Fatalf("expected check-violation for negative %s, got nil", col)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "check constraint") {
				t.Errorf("expected check-violation, got: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Coverage for the small RLS write path: a runtime caller scoped to
// tenant A cannot INSERT a row claiming tenant_id = B. Mirrors the
// canonical "WITH CHECK denies cross-tenant writes" assertion from
// ADR-0072.
// ---------------------------------------------------------------------------

// TestAIW1ARLS_RuntimeCannotWriteOtherTenant: WITH CHECK on the insert
// policy rejects rows whose tenant_id does not match the current GUC.
// Run against ai_policy and product as representative tables (the policy
// shape is identical across the four).
func TestAIW1ARLS_RuntimeCannotWriteOtherTenant(t *testing.T) {
	db, ctx := freshDBWithAIW1A(t)
	tenantA, _ := seedTenantUserMaster(t, db)
	tenantB := seedTenantB(t, ctx, db)

	cases := []struct {
		name string
		stmt string
		args []any
	}{
		{
			"ai_policy",
			`INSERT INTO ai_policy (tenant_id, scope_type, scope_id) VALUES ($1, 'tenant', $2)`,
			[]any{tenantB, tenantB.String()},
		},
		{
			"product",
			`INSERT INTO product (tenant_id, name) VALUES ($1, 'name')`,
			[]any{tenantB},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
				_, e := tx.Exec(ctx, tc.stmt, tc.args...)
				return e
			})
			if err == nil {
				t.Fatalf("%s: expected RLS-violation on cross-tenant write, got nil", tc.name)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "row-level security") &&
				!strings.Contains(strings.ToLower(err.Error()), "row level security") &&
				!strings.Contains(strings.ToLower(err.Error()), "violates row-level") {
				t.Errorf("%s: expected RLS-violation error, got: %v", tc.name, err)
			}
		})
	}
}
