package postgres_test

// SIN-62927 / Fase 3 decisão #8 acceptance for 0101_ai_policy_consent:
//
//   #1 up/down/up idempotent on the shared CI cluster
//   #2 RLS enabled on ai_policy_consent (relrowsecurity = true and
//      relforcerowsecurity = true)
//   #3 four tenant_isolation_{select,insert,update,delete} policies exist
//   #4 INSERT under WithTenant(A) cannot write a row claiming tenant_id = B
//      (RLS WITH CHECK violation)
//   #5 UNIQUE (tenant_id, scope_kind, scope_id) rejects a second row for
//      the same scope triple
//   #6 BEFORE INSERT OR UPDATE OR DELETE trigger
//      ai_policy_consent_master_ops_audit is registered
//
// Lives in postgres_test so it shares the cluster bootstrap state and
// avoids the SQLSTATE 28P01 race the mastersession pattern was built
// for (memory `testpg shared-cluster ALTER ROLE race`).

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

// freshDBWithAIPolicyConsent applies the minimum chain 0101 needs:
//   - 0004 tenants     (consent.tenant_id FK target)
//   - 0005 users       (consent.actor_user_id FK target)
//   - 0101             (the migration under test)
//
// master_ops_audit_trigger is created in 0002 (applied by the harness
// at DB-bootstrap time), so no extra step is needed for the trigger.
func freshDBWithAIPolicyConsent(t *testing.T) (*testpg.DB, context.Context) {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0101_ai_policy_consent.up.sql",
	)
	return db, ctx
}

func aiPolicyConsentTablePresent(t *testing.T, ctx context.Context, db *testpg.DB) bool {
	t.Helper()
	var n int
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relname = 'ai_policy_consent' AND n.nspname = 'public'`).Scan(&n); err != nil {
		t.Fatalf("table-exists probe: %v", err)
	}
	return n == 1
}

// samplePayloadHash returns a 32-byte digest the way the service layer
// will (SHA-256 of the anonymized preview). The test doesn't care about
// the cleartext — only that the column accepts 32-byte bytea values.
func samplePayloadHash(seed string) []byte {
	h := sha256.Sum256([]byte(seed))
	return h[:]
}

// ---------------------------------------------------------------------------
// AC #1 — up/down idempotency
// ---------------------------------------------------------------------------

func TestAIPolicyConsentMigration_UpDownUp(t *testing.T) {
	db, ctx := freshDBWithAIPolicyConsent(t)

	if !aiPolicyConsentTablePresent(t, ctx, db) {
		t.Fatalf("after initial up: ai_policy_consent table missing")
	}

	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(),
		"0101_ai_policy_consent.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(),
		"0101_ai_policy_consent.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}

	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if aiPolicyConsentTablePresent(t, ctx, db) {
		t.Fatalf("after down: ai_policy_consent still present")
	}

	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if !aiPolicyConsentTablePresent(t, ctx, db) {
		t.Fatalf("after re-up: ai_policy_consent missing")
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

// ---------------------------------------------------------------------------
// AC #2 — RLS enabled + FORCE
// ---------------------------------------------------------------------------

func TestAIPolicyConsent_RLSEnabledAndForced(t *testing.T) {
	db, ctx := freshDBWithAIPolicyConsent(t)

	var enabled, force bool
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class
		  WHERE relname = 'ai_policy_consent'`).Scan(&enabled, &force); err != nil {
		t.Fatalf("read pg_class flags: %v", err)
	}
	if !enabled {
		t.Errorf("relrowsecurity = false; want true")
	}
	if !force {
		t.Errorf("relforcerowsecurity = false; want true (ADR-0072)")
	}
}

// ---------------------------------------------------------------------------
// AC #3 — four tenant_isolation_* policies present
// ---------------------------------------------------------------------------

func TestAIPolicyConsent_TenantIsolationPoliciesPresent(t *testing.T) {
	db, ctx := freshDBWithAIPolicyConsent(t)

	rows, err := db.SuperuserPool().Query(ctx,
		`SELECT polname FROM pg_policy p
		   JOIN pg_class c ON c.oid = p.polrelid
		  WHERE c.relname = 'ai_policy_consent'
		    AND polname LIKE 'tenant_isolation_%'`)
	if err != nil {
		t.Fatalf("read pg_policy: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan polname: %v", err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	sort.Strings(got)
	want := []string{
		"tenant_isolation_delete",
		"tenant_isolation_insert",
		"tenant_isolation_select",
		"tenant_isolation_update",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d policies %v; want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("policy[%d]=%q; want %q", i, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// AC #4 — runtime under WithTenant(A) cannot write a row claiming tenant B
// ---------------------------------------------------------------------------

func TestAIPolicyConsent_RuntimeCannotWriteOtherTenant(t *testing.T) {
	db, ctx := freshDBWithAIPolicyConsent(t)
	tenantA, _ := seedTenantUserMaster(t, db)
	tenantB := seedTenantB(t, ctx, db)

	err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO ai_policy_consent
			   (tenant_id, scope_kind, scope_id, payload_hash,
			    anonymizer_version, prompt_version)
			 VALUES ($1, 'tenant', $2, $3, 'v1', 'v1')`,
			tenantB, tenantB.String(), samplePayloadHash("cross-tenant"))
		return e
	})
	if err == nil {
		t.Fatal("expected RLS-violation on cross-tenant INSERT, got nil")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "row-level security") &&
		!strings.Contains(msg, "row level security") &&
		!strings.Contains(msg, "violates row-level") {
		t.Errorf("expected RLS-violation error, got: %v", err)
	}
}

// TestAIPolicyConsent_TenantIsolation_OtherTenantSeesZero: a row seeded
// for tenant A is invisible to a runtime caller scoped to tenant B.
func TestAIPolicyConsent_TenantIsolation_OtherTenantSeesZero(t *testing.T) {
	db, ctx := freshDBWithAIPolicyConsent(t)
	tenantA, _ := seedTenantUserMaster(t, db)
	tenantB := seedTenantB(t, ctx, db)

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_policy_consent
		   (tenant_id, scope_kind, scope_id, payload_hash,
		    anonymizer_version, prompt_version)
		 VALUES ($1, 'tenant', $2, $3, 'v1', 'v1')`,
		tenantA, tenantA.String(), samplePayloadHash("A")); err != nil {
		t.Fatalf("seed consent for tenantA: %v", err)
	}

	var seen int
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ai_policy_consent`).Scan(&seen)
	}); err != nil {
		t.Fatalf("WithTenant(B) select: %v", err)
	}
	if seen != 0 {
		t.Errorf("tenant B saw %d ai_policy_consent rows; want 0", seen)
	}
}

// ---------------------------------------------------------------------------
// AC #5 — UNIQUE (tenant_id, scope_kind, scope_id)
// ---------------------------------------------------------------------------

func TestAIPolicyConsent_ScopeUniqueness(t *testing.T) {
	db, ctx := freshDBWithAIPolicyConsent(t)
	tenantA, _ := seedTenantUserMaster(t, db)

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_policy_consent
		   (tenant_id, scope_kind, scope_id, payload_hash,
		    anonymizer_version, prompt_version)
		 VALUES ($1, 'tenant', $2, $3, 'v1', 'v1')`,
		tenantA, tenantA.String(), samplePayloadHash("first")); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_policy_consent
		   (tenant_id, scope_kind, scope_id, payload_hash,
		    anonymizer_version, prompt_version)
		 VALUES ($1, 'tenant', $2, $3, 'v2', 'v2')`,
		tenantA, tenantA.String(), samplePayloadHash("second"))
	if err == nil {
		t.Fatal("expected unique-violation on duplicate scope triple, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}

	// Different scope_kind under the same tenant → accepted.
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_policy_consent
		   (tenant_id, scope_kind, scope_id, payload_hash,
		    anonymizer_version, prompt_version)
		 VALUES ($1, 'channel', 'whatsapp', $2, 'v1', 'v1')`,
		tenantA, samplePayloadHash("third")); err != nil {
		t.Errorf("different scope_kind rejected: %v", err)
	}
	// Same scope_kind, different scope_id → accepted.
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_policy_consent
		   (tenant_id, scope_kind, scope_id, payload_hash,
		    anonymizer_version, prompt_version)
		 VALUES ($1, 'channel', 'instagram', $2, 'v1', 'v1')`,
		tenantA, samplePayloadHash("fourth")); err != nil {
		t.Errorf("different scope_id rejected: %v", err)
	}
}

// TestAIPolicyConsent_ScopeKindCheck rejects scope_kind values outside
// {tenant, team, channel}.
func TestAIPolicyConsent_ScopeKindCheck(t *testing.T) {
	db, ctx := freshDBWithAIPolicyConsent(t)
	tenantA, _ := seedTenantUserMaster(t, db)

	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_policy_consent
		   (tenant_id, scope_kind, scope_id, payload_hash,
		    anonymizer_version, prompt_version)
		 VALUES ($1, 'global', $2, $3, 'v1', 'v1')`,
		tenantA, tenantA.String(), samplePayloadHash("bad"))
	if err == nil {
		t.Fatal("expected check-violation for scope_kind='global', got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "check constraint") &&
		!strings.Contains(strings.ToLower(err.Error()), "ai_policy_consent_scope_kind_check") {
		t.Errorf("expected scope_kind check-violation, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC #6 — master_ops audit trigger registered
// ---------------------------------------------------------------------------

func TestAIPolicyConsent_MasterOpsAuditTriggerRegistered(t *testing.T) {
	db, ctx := freshDBWithAIPolicyConsent(t)

	var name string
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT t.tgname FROM pg_trigger t
		   JOIN pg_class c ON c.oid = t.tgrelid
		  WHERE c.relname = 'ai_policy_consent'
		    AND t.tgname = 'ai_policy_consent_master_ops_audit'
		    AND NOT t.tgisinternal`).Scan(&name); err != nil {
		t.Fatalf("read trigger: %v", err)
	}
	if name != "ai_policy_consent_master_ops_audit" {
		t.Errorf("trigger name = %q; want ai_policy_consent_master_ops_audit", name)
	}
}

// ---------------------------------------------------------------------------
// Service contract — UPSERT path (re-consent rewrites the row, not a
// second INSERT). Captures the invariant the gate logic in W4-B will
// rely on so a future refactor can't silently weaken the column shape.
// ---------------------------------------------------------------------------

func TestAIPolicyConsent_UpsertReConsentMutatesSingleRow(t *testing.T) {
	db, ctx := freshDBWithAIPolicyConsent(t)
	tenantA, _ := seedTenantUserMaster(t, db)

	const stmt = `
		INSERT INTO ai_policy_consent
		  (tenant_id, scope_kind, scope_id, payload_hash,
		   anonymizer_version, prompt_version)
		VALUES ($1, 'tenant', $2, $3, $4, $5)
		ON CONFLICT (tenant_id, scope_kind, scope_id) DO UPDATE
		  SET payload_hash       = EXCLUDED.payload_hash,
		      anonymizer_version = EXCLUDED.anonymizer_version,
		      prompt_version     = EXCLUDED.prompt_version,
		      accepted_at        = now()`

	if _, err := db.AdminPool().Exec(ctx, stmt,
		tenantA, tenantA.String(), samplePayloadHash("v1"), "anon-v1", "prompt-v1"); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}
	// Re-consent under a newer anonymizer + prompt version: same row,
	// updated columns.
	if _, err := db.AdminPool().Exec(ctx, stmt,
		tenantA, tenantA.String(), samplePayloadHash("v2"), "anon-v2", "prompt-v2"); err != nil {
		t.Fatalf("re-consent upsert: %v", err)
	}

	var rowCount int
	var anonVersion, promptVersion string
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM ai_policy_consent WHERE tenant_id = $1`,
		tenantA).Scan(&rowCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("expected 1 row after re-consent UPSERT, got %d", rowCount)
	}
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT anonymizer_version, prompt_version FROM ai_policy_consent
		  WHERE tenant_id = $1 AND scope_kind = 'tenant' AND scope_id = $2`,
		tenantA, tenantA.String()).Scan(&anonVersion, &promptVersion); err != nil {
		t.Fatalf("read versions: %v", err)
	}
	if anonVersion != "anon-v2" || promptVersion != "prompt-v2" {
		t.Errorf("post-upsert versions = (%s, %s); want (anon-v2, prompt-v2)",
			anonVersion, promptVersion)
	}
}

// ---------------------------------------------------------------------------
// Column shape sanity — guards against a future migration that quietly
// changes a type or drops the NOT NULL on payload_hash/version columns.
// information_schema.columns is the canonical source.
// ---------------------------------------------------------------------------

func TestAIPolicyConsent_ColumnShape(t *testing.T) {
	db, ctx := freshDBWithAIPolicyConsent(t)

	type col struct {
		dataType string
		nullable string
	}
	want := map[string]col{
		"id":                 {"uuid", "NO"},
		"tenant_id":          {"uuid", "NO"},
		"scope_kind":         {"text", "NO"},
		"scope_id":           {"text", "NO"},
		"actor_user_id":      {"uuid", "YES"},
		"payload_hash":       {"bytea", "NO"},
		"anonymizer_version": {"text", "NO"},
		"prompt_version":     {"text", "NO"},
		"accepted_at":        {"timestamp with time zone", "NO"},
	}

	rows, err := db.SuperuserPool().Query(ctx,
		`SELECT column_name, data_type, is_nullable
		   FROM information_schema.columns
		  WHERE table_schema = 'public'
		    AND table_name = 'ai_policy_consent'`)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	defer rows.Close()
	got := make(map[string]col)
	for rows.Next() {
		var name string
		var c col
		if err := rows.Scan(&name, &c.dataType, &c.nullable); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(got) != len(want) {
		t.Errorf("got %d columns; want %d (got=%v, want=%v)",
			len(got), len(want), got, want)
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("missing column %q", name)
			continue
		}
		if g.dataType != w.dataType {
			t.Errorf("column %q: data_type=%q; want %q", name, g.dataType, w.dataType)
		}
		if g.nullable != w.nullable {
			t.Errorf("column %q: is_nullable=%q; want %q", name, g.nullable, w.nullable)
		}
	}
}

// ---------------------------------------------------------------------------
// FK behaviour — deleting the tenant cascades; deleting the actor user
// nulls actor_user_id rather than deleting the consent.
// ---------------------------------------------------------------------------

func TestAIPolicyConsent_CascadeOnTenantDelete(t *testing.T) {
	db, ctx := freshDBWithAIPolicyConsent(t)
	tenantA, _ := seedTenantUserMaster(t, db)

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_policy_consent
		   (tenant_id, scope_kind, scope_id, payload_hash,
		    anonymizer_version, prompt_version)
		 VALUES ($1, 'tenant', $2, $3, 'v1', 'v1')`,
		tenantA, tenantA.String(), samplePayloadHash("c")); err != nil {
		t.Fatalf("seed consent: %v", err)
	}

	if _, err := db.AdminPool().Exec(ctx,
		`DELETE FROM tenants WHERE id = $1`, tenantA); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}

	var n int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM ai_policy_consent WHERE tenant_id = $1`,
		tenantA).Scan(&n); err != nil {
		t.Fatalf("count after tenant delete: %v", err)
	}
	if n != 0 {
		t.Errorf("tenant delete left %d consent rows; want 0", n)
	}
}

func TestAIPolicyConsent_SetNullOnActorDelete(t *testing.T) {
	db, ctx := freshDBWithAIPolicyConsent(t)
	tenantA, _ := seedTenantUserMaster(t, db)
	actorID := seedAIPolicyConsentActor(t, ctx, db, tenantA)

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO ai_policy_consent
		   (tenant_id, scope_kind, scope_id, actor_user_id, payload_hash,
		    anonymizer_version, prompt_version)
		 VALUES ($1, 'tenant', $2, $3, $4, 'v1', 'v1')`,
		tenantA, tenantA.String(), actorID, samplePayloadHash("c")); err != nil {
		t.Fatalf("seed consent: %v", err)
	}

	if _, err := db.AdminPool().Exec(ctx,
		`DELETE FROM users WHERE id = $1`, actorID); err != nil {
		t.Fatalf("delete actor user: %v", err)
	}

	var actor *uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT actor_user_id FROM ai_policy_consent
		  WHERE tenant_id = $1 AND scope_kind = 'tenant' AND scope_id = $2`,
		tenantA, tenantA.String()).Scan(&actor); err != nil {
		t.Fatalf("read consent after user delete: %v", err)
	}
	if actor != nil {
		t.Errorf("actor_user_id = %v after user delete; want NULL", actor)
	}
}

// seedAIPolicyConsentActor inserts a tenant-bound non-master user. The
// consent test's actor_user_id has ON DELETE SET NULL, so we exercise
// that path with a real user row.
func seedAIPolicyConsentActor(t *testing.T, ctx context.Context, db *testpg.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	userID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, $2, $3, 'x', 'tenant_gerente', false)`,
		userID, tenantID, fmt.Sprintf("consent-actor-%s@x", userID)); err != nil {
		t.Fatalf("seed actor user: %v", err)
	}
	return userID
}
