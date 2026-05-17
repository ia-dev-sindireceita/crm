package postgres_test

// SIN-62952 / Fase 4 acceptance for 0101_phase4_marketing_billing_dunning:
//
//   #1 up/down/up idempotent on the shared CI cluster (all seven tables
//      appear after up, disappear after down, and can re-up cleanly).
//   #2 RLS policies isolate tenanted tables (campaigns, campaign_clicks,
//      funnel_rules, pix_charges, subscription_dunning_states) by tenant
//      and force-apply to the owner role.
//   #3 webhook_events is intentionally NOT tenant-scoped — only the
//      (source, external_id, event_type) UNIQUE dedup contract is
//      enforced; the second insert of the same triple raises a unique
//      violation.
//   #4 campaigns enforces UNIQUE (tenant_id, slug); two tenants can both
//      own the same slug.
//   #5 campaign_clicks.click_id is globally UNIQUE (idempotency under
//      browser retry).
//   #6 pix_charges paid_at/status pairing CHECK and partial UNIQUE on
//      external_id (NULL allowed during pre-ack window).
//   #7 subscription_dunning_states state CHECK rejects unknown values
//      and override_until/override_reason consistency.
//   #8 seed/token_packages.sql is idempotent and writes exactly the
//      D4 catalogue (small / medium / large with the ratified prices).
//
// Tests live in the parent postgres_test package (not a per-table
// subpackage or a new test binary) so they share the shared-cluster
// ALTER ROLE bootstrap and don't race the SQLSTATE 28P01 regression
// pattern from SIN-62726 / SIN-62750.

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

// phase4TableNames lists every table created by 0101.
var phase4TableNames = []string{
	"campaigns",
	"campaign_clicks",
	"funnel_rules",
	"pix_charges",
	"subscription_dunning_states",
	"webhook_events",
	"token_packages",
}

// freshDBWithPhase4 layers Fase 4's prerequisites on top of the bootstrap
// migrations the testpg harness applies (0001-0003): tenants, users,
// inbox/contacts, wallet, subscription/billing, then this migration.
// Mirrors the freshDBWithBilling helper pattern.
func freshDBWithPhase4(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0089_wallet_basic.up.sql",
		"0097_subscription_plan_invoice_master_grant.up.sql",
		"0101_phase4_marketing_billing_dunning.up.sql",
	} {
		path := filepath.Join(harness.MigrationsDir(), name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := db.AdminPool().Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	return db
}

func phase4TablesPresent(t *testing.T, ctx context.Context, db *testpg.DB) int {
	t.Helper()
	var count int
	row := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relname = ANY($1)
		    AND n.nspname = 'public'`, phase4TableNames)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("phase4-tables probe: %v", err)
	}
	return count
}

// seedActivePhase4Subscription is the dunning-test analogue of
// seedActiveSubscription. It seeds plan + subscription so dunning rows
// have a valid FK target.
func seedActivePhase4Subscription(t *testing.T, ctx context.Context, db *testpg.DB, tenantID, masterID uuid.UUID) uuid.UUID {
	t.Helper()
	planID := seedPlan(t, ctx, db, fmt.Sprintf("phase4-%s", uuid.NewString()[:8]), 1_000_000)
	return seedActiveSubscription(t, ctx, db, tenantID, planID, masterID)
}

// ---------------------------------------------------------------------------
// AC #1 — up/down/up idempotency
// ---------------------------------------------------------------------------

func TestPhase4Migration_UpDownUp(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if got := phase4TablesPresent(t, ctx, db); got != len(phase4TableNames) {
		t.Fatalf("after initial up: got %d/%d phase4 tables", got, len(phase4TableNames))
	}

	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(),
		"0101_phase4_marketing_billing_dunning.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if got := phase4TablesPresent(t, ctx, db); got != 0 {
		t.Fatalf("after down: %d/%d phase4 tables still present", got, len(phase4TableNames))
	}

	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(),
		"0101_phase4_marketing_billing_dunning.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if got := phase4TablesPresent(t, ctx, db); got != len(phase4TableNames) {
		t.Fatalf("after re-up: got %d/%d phase4 tables", got, len(phase4TableNames))
	}

	// Down-twice and up-twice must both be no-ops.
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

// ---------------------------------------------------------------------------
// AC #2 — RLS isolation across tenants
// ---------------------------------------------------------------------------

func TestPhase4RLS_CampaignsTenantIsolation(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, _ := seedTenantUserMaster(t, db)
	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "tenantB", fmt.Sprintf("b-%s.crm.local", tenantB)); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO campaigns (tenant_id, name, slug, redirect_url)
		 VALUES ($1, 'A camp', 'sample', 'https://a.example/x'),
		        ($2, 'B camp', 'sample', 'https://b.example/x')`,
		tenantA, tenantB); err != nil {
		t.Fatalf("seed campaigns: %v", err)
	}

	var seen []uuid.UUID
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id FROM campaigns`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tid uuid.UUID
			if err := rows.Scan(&tid); err != nil {
				return err
			}
			seen = append(seen, tid)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("WithTenant(A): %v", err)
	}
	if len(seen) != 1 || seen[0] != tenantA {
		t.Fatalf("tenant A sees %v, want [%s]", seen, tenantA)
	}
}

func TestPhase4ForceRLS_AppliesToOwner(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenanted := []string{
		"campaigns",
		"campaign_clicks",
		"funnel_rules",
		"pix_charges",
		"subscription_dunning_states",
	}
	for _, table := range tenanted {
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

func TestPhase4RLS_NoTenantSetReturnsZero(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, _ := seedTenantUserMaster(t, db)
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO campaigns (tenant_id, name, slug, redirect_url)
		 VALUES ($1, 'A', 'evergreen', 'https://example/x')`, tenantA); err != nil {
		t.Fatalf("seed campaign: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO funnel_rules (tenant_id, name, trigger_type, action_type)
		 VALUES ($1, 'auto-handoff', 'message_contains', 'move_to_stage')`, tenantA); err != nil {
		t.Fatalf("seed funnel rule: %v", err)
	}

	for _, table := range []string{"campaigns", "funnel_rules"} {
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
// AC #3 — webhook_events dedup (no tenant scope)
// ---------------------------------------------------------------------------

func TestWebhookEvents_DedupUniqueness(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO webhook_events (source, external_id, event_type, payload)
		 VALUES ('psp-x', 'evt-1', 'charge.paid', '{"ok":true}'::jsonb)`); err != nil {
		t.Fatalf("first webhook insert: %v", err)
	}

	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO webhook_events (source, external_id, event_type, payload)
		 VALUES ('psp-x', 'evt-1', 'charge.paid', '{"ok":true}'::jsonb)`)
	if err == nil {
		t.Fatal("expected UNIQUE violation on duplicate (source,external_id,event_type), got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}

	// A different event_type for the same external_id is allowed —
	// PSPs commonly emit `charge.created` then `charge.paid` for the
	// same external id.
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO webhook_events (source, external_id, event_type, payload)
		 VALUES ('psp-x', 'evt-1', 'charge.created', '{"ok":true}'::jsonb)`); err != nil {
		t.Fatalf("distinct event_type insert: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC #4 — campaigns slug uniqueness is per-tenant
// ---------------------------------------------------------------------------

func TestCampaigns_SlugUniquePerTenant(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, _ := seedTenantUserMaster(t, db)
	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "tenantB", fmt.Sprintf("b-%s.crm.local", tenantB)); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO campaigns (tenant_id, name, slug, redirect_url)
		 VALUES ($1, 'A', 'shared-slug', 'https://a/x'),
		        ($2, 'B', 'shared-slug', 'https://b/x')`,
		tenantA, tenantB); err != nil {
		t.Fatalf("cross-tenant same slug should be allowed: %v", err)
	}

	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO campaigns (tenant_id, name, slug, redirect_url)
		 VALUES ($1, 'A dup', 'shared-slug', 'https://a/y')`, tenantA)
	if err == nil {
		t.Fatal("expected UNIQUE (tenant_id, slug) violation, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC #5 — campaign_clicks click_id is globally UNIQUE
// ---------------------------------------------------------------------------

func TestCampaignClicks_ClickIDUnique(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, _ := seedTenantUserMaster(t, db)
	var campID uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`INSERT INTO campaigns (tenant_id, name, slug, redirect_url)
		 VALUES ($1, 'A', 'campA', 'https://a/x') RETURNING id`, tenantA).Scan(&campID); err != nil {
		t.Fatalf("seed campaign: %v", err)
	}

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO campaign_clicks (tenant_id, campaign_id, click_id)
		 VALUES ($1, $2, 'browser-click-1')`, tenantA, campID); err != nil {
		t.Fatalf("first click insert: %v", err)
	}

	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO campaign_clicks (tenant_id, campaign_id, click_id)
		 VALUES ($1, $2, 'browser-click-1')`, tenantA, campID)
	if err == nil {
		t.Fatal("expected UNIQUE violation on duplicate click_id, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC #6 — pix_charges paid_at/status consistency + external_id partial UNIQUE
// ---------------------------------------------------------------------------

func TestPixCharges_PaidAtConsistencyCheck(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	subID := seedActivePhase4Subscription(t, ctx, db, tenantA, masterID)

	var invID uuid.UUID
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO invoice (tenant_id, subscription_id, period_start, period_end,
			                       amount_cents_brl, state)
			 VALUES ($1, $2, current_date, current_date + 30, 9900, 'pending')
			 RETURNING id`, tenantA, subID).Scan(&invID)
	}); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}

	// status='paid' without paid_at violates CHECK.
	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO pix_charges
		   (tenant_id, invoice_id, qr_code, copy_paste, status, expires_at)
		 VALUES ($1, $2, 'qr', 'cp', 'paid', now() + interval '1 hour')`,
		tenantA, invID)
	if err == nil {
		t.Fatal("expected paid_at consistency CHECK to reject status=paid without paid_at, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "pix_charges_paid_at_consistency") {
		t.Errorf("expected paid_at consistency check error, got: %v", err)
	}

	// Two pending charges with NULL external_id are allowed (partial UNIQUE).
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO pix_charges
		   (tenant_id, invoice_id, qr_code, copy_paste, expires_at)
		 VALUES ($1, $2, 'qr1', 'cp1', now() + interval '1 hour'),
		        ($1, $2, 'qr2', 'cp2', now() + interval '1 hour')`,
		tenantA, invID); err != nil {
		t.Fatalf("two NULL external_id rows should be allowed: %v", err)
	}

	// Duplicate non-NULL external_id rejected.
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO pix_charges
		   (tenant_id, invoice_id, external_id, qr_code, copy_paste, expires_at)
		 VALUES ($1, $2, 'psp-extid-1', 'qr', 'cp', now() + interval '1 hour')`,
		tenantA, invID); err != nil {
		t.Fatalf("first non-NULL external_id insert: %v", err)
	}
	_, err = db.AdminPool().Exec(ctx,
		`INSERT INTO pix_charges
		   (tenant_id, invoice_id, external_id, qr_code, copy_paste, expires_at)
		 VALUES ($1, $2, 'psp-extid-1', 'qr', 'cp', now() + interval '1 hour')`,
		tenantA, invID)
	if err == nil {
		t.Fatal("expected partial UNIQUE on external_id to reject duplicate, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC #7 — subscription_dunning_states CHECK constraints
// ---------------------------------------------------------------------------

func TestSubscriptionDunning_StateAndOverrideChecks(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, masterID := seedTenantUserMaster(t, db)
	subID := seedActivePhase4Subscription(t, ctx, db, tenantA, masterID)

	// Unknown state value rejected.
	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO subscription_dunning_states
		   (tenant_id, subscription_id, state)
		 VALUES ($1, $2, 'frozen')`, tenantA, subID)
	if err == nil {
		t.Fatal("expected CHECK violation on invalid state, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "check constraint") {
		t.Errorf("expected check constraint error, got: %v", err)
	}

	// override_until without override_reason rejected.
	_, err = db.AdminPool().Exec(ctx,
		`INSERT INTO subscription_dunning_states
		   (tenant_id, subscription_id, state, override_until)
		 VALUES ($1, $2, 'warn', now() + interval '1 day')`, tenantA, subID)
	if err == nil {
		t.Fatal("expected override consistency CHECK, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "override_consistency") {
		t.Errorf("expected override consistency error, got: %v", err)
	}

	// override_reason shorter than 10 chars rejected.
	_, err = db.AdminPool().Exec(ctx,
		`INSERT INTO subscription_dunning_states
		   (tenant_id, subscription_id, state, override_until, override_reason)
		 VALUES ($1, $2, 'warn', now() + interval '1 day', 'short')`, tenantA, subID)
	if err == nil {
		t.Fatal("expected override_reason length CHECK, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "override_consistency") {
		t.Errorf("expected override consistency error, got: %v", err)
	}

	// Valid row accepted.
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO subscription_dunning_states
		   (tenant_id, subscription_id, state, override_until, override_reason)
		 VALUES ($1, $2, 'warn', now() + interval '1 day', 'manual reprieve granted')`,
		tenantA, subID); err != nil {
		t.Fatalf("valid override row rejected: %v", err)
	}

	// UNIQUE(subscription_id): a second dunning row for the same subscription
	// is rejected so each subscription has exactly one tracked state.
	_, err = db.AdminPool().Exec(ctx,
		`INSERT INTO subscription_dunning_states
		   (tenant_id, subscription_id, state)
		 VALUES ($1, $2, 'current')`, tenantA, subID)
	if err == nil {
		t.Fatal("expected UNIQUE(subscription_id) violation, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC #8 — seed/token_packages.sql reproduces the D4 table exactly
// ---------------------------------------------------------------------------

func TestTokenPackagesSeed_ReproducesD4Catalogue(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	seed, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "seed", "token_packages.sql"))
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	// Apply twice to confirm idempotency (ON CONFLICT (slug) DO NOTHING).
	for i := 0; i < 2; i++ {
		if _, err := db.AdminPool().Exec(ctx, string(seed)); err != nil {
			t.Fatalf("apply seed (run %d): %v", i, err)
		}
	}

	// Exact match against the D4 ratified table:
	//   small  | 1_000_000   | R$ 15  → 1500 cents
	//   medium | 5_000_000   | R$ 49  → 4900 cents
	//   large  | 20_000_000  | R$ 149 → 14900 cents
	want := map[string]struct {
		tokens int64
		cents  int
	}{
		"small":  {tokens: 1_000_000, cents: 1500},
		"medium": {tokens: 5_000_000, cents: 4900},
		"large":  {tokens: 20_000_000, cents: 14900},
	}

	rows, err := db.AdminPool().Query(ctx,
		`SELECT slug, kind, tokens, price_cents_brl
		   FROM token_packages
		  WHERE slug = ANY($1)
		  ORDER BY slug`,
		[]string{"small", "medium", "large"})
	if err != nil {
		t.Fatalf("query token_packages: %v", err)
	}
	defer rows.Close()

	got := map[string]struct {
		kind   string
		tokens int64
		cents  int
	}{}
	for rows.Next() {
		var slug, kind string
		var tokens int64
		var cents int
		if err := rows.Scan(&slug, &kind, &tokens, &cents); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[slug] = struct {
			kind   string
			tokens int64
			cents  int
		}{kind, tokens, cents}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	for slug, w := range want {
		g, ok := got[slug]
		if !ok {
			t.Errorf("seed missing %s", slug)
			continue
		}
		if g.kind != "tokens" {
			t.Errorf("%s.kind = %q, want %q", slug, g.kind, "tokens")
		}
		if g.tokens != w.tokens {
			t.Errorf("%s.tokens = %d, want %d", slug, g.tokens, w.tokens)
		}
		if g.cents != w.cents {
			t.Errorf("%s.price_cents_brl = %d, want %d", slug, g.cents, w.cents)
		}
	}
}

// TestTokenPackages_SlugUnique: the catalogue's slug UNIQUE prevents
// duplicate seeded packages across re-runs of the seed file.
func TestTokenPackages_SlugUnique(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO token_packages (slug, kind, name, tokens, price_cents_brl)
		 VALUES ('xs', 'tokens', 'Extra Small', 1, 1)`); err != nil {
		t.Fatalf("first token_packages insert: %v", err)
	}
	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO token_packages (slug, kind, name, tokens, price_cents_brl)
		 VALUES ('xs', 'tokens', 'Extra Small Dup', 1, 1)`)
	if err == nil {
		t.Fatal("expected unique-violation for duplicate token_packages slug, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}
}
