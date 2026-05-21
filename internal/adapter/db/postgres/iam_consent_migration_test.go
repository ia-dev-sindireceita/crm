package postgres_test

// SIN-63185 / Fase 6 PR2 acceptance for migration
// 0113_consent_record (originally numbered 0107, renumbered by
// SIN-63230 to resolve a three-way collision on 0107):
//
//   #1 up/down/up idempotent on the shared CI cluster
//   #2 RLS enabled on consent_record (relrowsecurity = true and
//      relforcerowsecurity = true)
//   #3 four tenant_isolation_{select,insert,update,delete} policies
//      exist on consent_record
//   #4 INSERT under WithTenant(A) cannot write a row claiming
//      tenant_id = B (RLS WITH CHECK violation)
//   #5 UNIQUE (tenant_id, subject_type, subject_id, purpose, version)
//      rejects a duplicate triple
//   #6 BEFORE INSERT OR UPDATE OR DELETE trigger
//      consent_record_master_ops_audit is registered
//   #7 audit_log_data accepts 'consent_grant' and 'consent_revoke'
//      after the migration applies; rejects 'bogus_event'
//
// Lives in postgres_test so it shares the cluster bootstrap state
// (see memory `testpg shared-cluster ALTER ROLE race`).

import (
	"context"
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

// freshDBWithConsentRecord applies the minimum chain 0113 needs:
//   - 0004 tenants            (consent_record.tenant_id FK target)
//   - 0005 users              (audit context for master_ops_audit
//     trigger lineage)
//   - 0083 split audit tables (audit_log_data's CHECK clause is
//     extended by 0113)
//   - 0113 consent_record     (the migration under test)
//
// 0083 itself needs only the harness default 0001-0003 plus 0004 +
// 0005 (it references tenants/users via FK). master_ops_audit_trigger
// is created in 0002 by the harness.
func freshDBWithConsentRecord(t *testing.T) (*testpg.DB, context.Context) {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0083_split_audit_log.up.sql",
		"0113_consent_record.up.sql",
	)
	return db, ctx
}

func consentRecordTablePresent(t *testing.T, ctx context.Context, db *testpg.DB) bool {
	t.Helper()
	var n int
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relname = 'consent_record' AND n.nspname = 'public'`).Scan(&n); err != nil {
		t.Fatalf("table-exists probe: %v", err)
	}
	return n == 1
}

// ---------------------------------------------------------------------------
// AC #1 — up/down/up idempotency
// ---------------------------------------------------------------------------

func TestConsentRecordMigration_UpDownUp(t *testing.T) {
	db, ctx := freshDBWithConsentRecord(t)

	if !consentRecordTablePresent(t, ctx, db) {
		t.Fatalf("after initial up: consent_record table missing")
	}

	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(),
		"0113_consent_record.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(),
		"0113_consent_record.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}

	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if consentRecordTablePresent(t, ctx, db) {
		t.Fatalf("after down: consent_record still present")
	}

	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if !consentRecordTablePresent(t, ctx, db) {
		t.Fatalf("after re-up: consent_record missing")
	}

	// down-twice and up-twice are no-ops without error.
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

func TestConsentRecord_RLSEnabledAndForced(t *testing.T) {
	db, ctx := freshDBWithConsentRecord(t)

	var enabled, force bool
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class
		  WHERE relname = 'consent_record'`).Scan(&enabled, &force); err != nil {
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

func TestConsentRecord_TenantIsolationPoliciesPresent(t *testing.T) {
	db, ctx := freshDBWithConsentRecord(t)

	rows, err := db.SuperuserPool().Query(ctx,
		`SELECT polname FROM pg_policy p
		   JOIN pg_class c ON c.oid = p.polrelid
		  WHERE c.relname = 'consent_record'
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
// AC #4 — RLS cross-tenant write protection
// ---------------------------------------------------------------------------

func TestConsentRecord_RuntimeCannotWriteOtherTenant(t *testing.T) {
	db, ctx := freshDBWithConsentRecord(t)
	tenantA, _ := seedTenantUserMaster(t, db)
	tenantB := seedTenantBForConsent(t, ctx, db)

	err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO consent_record
			   (tenant_id, subject_type, subject_id, purpose, version)
			 VALUES ($1, 'user', $2, 'terms_of_service', 'v1')`,
			tenantB, "u-x")
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

func TestConsentRecord_TenantIsolation_OtherTenantSeesZero(t *testing.T) {
	db, ctx := freshDBWithConsentRecord(t)
	tenantA, _ := seedTenantUserMaster(t, db)
	tenantB := seedTenantBForConsent(t, ctx, db)

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO consent_record
		   (tenant_id, subject_type, subject_id, purpose, version)
		 VALUES ($1, 'user', 'u-a', 'terms_of_service', 'v1')`,
		tenantA); err != nil {
		t.Fatalf("seed consent for tenantA: %v", err)
	}

	var seen int
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM consent_record`).Scan(&seen)
	}); err != nil {
		t.Fatalf("WithTenant(B) select: %v", err)
	}
	if seen != 0 {
		t.Errorf("tenant B saw %d consent_record rows; want 0", seen)
	}
}

// ---------------------------------------------------------------------------
// AC #5 — UNIQUE (tenant_id, subject_type, subject_id, purpose, version)
// ---------------------------------------------------------------------------

func TestConsentRecord_VersionUniqueness(t *testing.T) {
	db, ctx := freshDBWithConsentRecord(t)
	tenantA, _ := seedTenantUserMaster(t, db)

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO consent_record
		   (tenant_id, subject_type, subject_id, purpose, version)
		 VALUES ($1, 'user', 'u-1', 'terms_of_service', 'v1')`,
		tenantA); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO consent_record
		   (tenant_id, subject_type, subject_id, purpose, version)
		 VALUES ($1, 'user', 'u-1', 'terms_of_service', 'v1')`,
		tenantA)
	if err == nil {
		t.Fatal("expected unique-violation on duplicate quintuple, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}

	// Different version is accepted (new row per version).
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO consent_record
		   (tenant_id, subject_type, subject_id, purpose, version)
		 VALUES ($1, 'user', 'u-1', 'terms_of_service', 'v2')`,
		tenantA); err != nil {
		t.Errorf("new version rejected: %v", err)
	}
	// Different purpose is accepted.
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO consent_record
		   (tenant_id, subject_type, subject_id, purpose, version)
		 VALUES ($1, 'user', 'u-1', 'marketing', 'v1')`,
		tenantA); err != nil {
		t.Errorf("different purpose rejected: %v", err)
	}
}

// TestConsentRecord_PurposeAndSubjectTypeChecks reject values outside
// the controlled vocabularies the migration installs.
func TestConsentRecord_PurposeAndSubjectTypeChecks(t *testing.T) {
	db, ctx := freshDBWithConsentRecord(t)
	tenantA, _ := seedTenantUserMaster(t, db)

	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO consent_record
		   (tenant_id, subject_type, subject_id, purpose, version)
		 VALUES ($1, 'admin', 'u-1', 'terms_of_service', 'v1')`,
		tenantA)
	if err == nil {
		t.Fatal("expected check-violation for subject_type='admin', got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "check constraint") {
		t.Errorf("expected subject_type check-violation, got: %v", err)
	}

	_, err = db.AdminPool().Exec(ctx,
		`INSERT INTO consent_record
		   (tenant_id, subject_type, subject_id, purpose, version)
		 VALUES ($1, 'user', 'u-1', 'ai_consent', 'v1')`,
		tenantA)
	if err == nil {
		t.Fatal("expected check-violation for purpose='ai_consent', got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "check constraint") {
		t.Errorf("expected purpose check-violation, got: %v", err)
	}
}

// TestConsentRecord_RevokeConsistencyCheck enforces that granted/
// revoked_at/revoke_reason cannot be in inconsistent combinations.
func TestConsentRecord_RevokeConsistencyCheck(t *testing.T) {
	db, ctx := freshDBWithConsentRecord(t)
	tenantA, _ := seedTenantUserMaster(t, db)

	// granted=true with revoked_at set is rejected.
	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO consent_record
		   (tenant_id, subject_type, subject_id, purpose, version,
		    granted, revoked_at)
		 VALUES ($1, 'user', 'u-1', 'terms_of_service', 'v1', true, now())`,
		tenantA)
	if err == nil {
		t.Errorf("expected consistency-check violation for granted=true with revoked_at, got nil")
	}
	// granted=false without revoked_at is rejected.
	_, err = db.AdminPool().Exec(ctx,
		`INSERT INTO consent_record
		   (tenant_id, subject_type, subject_id, purpose, version,
		    granted, revoked_at)
		 VALUES ($1, 'user', 'u-2', 'terms_of_service', 'v1', false, NULL)`,
		tenantA)
	if err == nil {
		t.Errorf("expected consistency-check violation for granted=false without revoked_at, got nil")
	}
}

// ---------------------------------------------------------------------------
// AC #6 — master_ops audit trigger registered
// ---------------------------------------------------------------------------

func TestConsentRecord_MasterOpsAuditTriggerRegistered(t *testing.T) {
	db, ctx := freshDBWithConsentRecord(t)

	var name string
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT t.tgname FROM pg_trigger t
		   JOIN pg_class c ON c.oid = t.tgrelid
		  WHERE c.relname = 'consent_record'
		    AND t.tgname = 'consent_record_master_ops_audit'
		    AND NOT t.tgisinternal`).Scan(&name); err != nil {
		t.Fatalf("read trigger: %v", err)
	}
	if name != "consent_record_master_ops_audit" {
		t.Errorf("trigger name = %q; want consent_record_master_ops_audit", name)
	}
}

// ---------------------------------------------------------------------------
// AC #7 — audit_log_data event_type CHECK extends to consent events
// ---------------------------------------------------------------------------

func TestConsentRecord_AuditLogDataAcceptsConsentEvents(t *testing.T) {
	db, ctx := freshDBWithConsentRecord(t)
	tenantA, masterID := seedTenantUserMaster(t, db)

	for _, ev := range []string{"consent_grant", "consent_revoke"} {
		if _, err := db.AdminPool().Exec(ctx,
			`INSERT INTO audit_log_data (tenant_id, actor_user_id, event_type, target)
			 VALUES ($1, $2, $3, '{}'::jsonb)`,
			tenantA, masterID, ev); err != nil {
			t.Errorf("insert audit_log_data with event_type=%q: %v", ev, err)
		}
	}

	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO audit_log_data (tenant_id, actor_user_id, event_type, target)
		 VALUES ($1, $2, 'bogus_event', '{}'::jsonb)`,
		tenantA, masterID)
	if err == nil {
		t.Fatal("expected check-violation for bogus event_type, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "check constraint") {
		t.Errorf("expected event_type check-violation, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Column shape sanity
// ---------------------------------------------------------------------------

func TestConsentRecord_ColumnShape(t *testing.T) {
	db, ctx := freshDBWithConsentRecord(t)

	type col struct {
		dataType string
		nullable string
	}
	want := map[string]col{
		"id":            {"uuid", "NO"},
		"tenant_id":     {"uuid", "NO"},
		"subject_type":  {"text", "NO"},
		"subject_id":    {"text", "NO"},
		"purpose":       {"text", "NO"},
		"version":       {"text", "NO"},
		"granted":       {"boolean", "NO"},
		"granted_at":    {"timestamp with time zone", "NO"},
		"revoked_at":    {"timestamp with time zone", "YES"},
		"revoke_reason": {"text", "YES"},
		"ip":            {"inet", "YES"},
		"user_agent":    {"text", "YES"},
	}

	rows, err := db.SuperuserPool().Query(ctx,
		`SELECT column_name, data_type, is_nullable
		   FROM information_schema.columns
		  WHERE table_schema = 'public'
		    AND table_name = 'consent_record'`)
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

// TestConsentRecord_CascadeOnTenantDelete ensures FK ON DELETE CASCADE
// removes consent rows when their owning tenant is deleted.
func TestConsentRecord_CascadeOnTenantDelete(t *testing.T) {
	db, ctx := freshDBWithConsentRecord(t)
	tenantA, _ := seedTenantUserMaster(t, db)

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO consent_record
		   (tenant_id, subject_type, subject_id, purpose, version)
		 VALUES ($1, 'user', 'u-1', 'terms_of_service', 'v1')`,
		tenantA); err != nil {
		t.Fatalf("seed consent: %v", err)
	}

	if _, err := db.AdminPool().Exec(ctx,
		`DELETE FROM tenants WHERE id = $1`, tenantA); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}

	var n int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM consent_record WHERE tenant_id = $1`,
		tenantA).Scan(&n); err != nil {
		t.Fatalf("count after tenant delete: %v", err)
	}
	if n != 0 {
		t.Errorf("tenant delete left %d consent rows; want 0", n)
	}
}

// seedTenantBForConsent inserts a second tenant for cross-tenant RLS
// tests. Defined here rather than reusing seedTenantB (which lives in
// ai_policy_summary_product_migration_test.go) so the consent test
// stays self-contained when batches are re-landed independently.
func seedTenantBForConsent(t *testing.T, ctx context.Context, db *testpg.DB) uuid.UUID {
	t.Helper()
	tenantID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host)
		 VALUES ($1, 'consent-tenant-b', $2)`,
		tenantID, "consent-b-"+tenantID.String()+".crm.local"); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}
	return tenantID
}
