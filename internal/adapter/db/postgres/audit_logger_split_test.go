package postgres_test

// SIN-62252: integration tests for the postgres SplitAuditLogger
// adapter and migrations 0012/0013/0014 (split-audit tables, tenant
// retention column, app_audit grant extension).
//
// Tests apply migrations 0004 (tenants) → 0005 (users) → 0007
// (legacy audit_log) → 0009 (app_audit role) → 0012 (split tables) →
// 0013 (tenants.audit_data_retention_months) → 0014 (app_audit grants
// on split tables) on top of the harness's default 0001-0003 sequence,
// then exercise SplitAuditLogger end-to-end through the dedicated
// app_audit pool.
//
// The legacy audit_log path is covered by audit_logger_test.go; the
// split path is covered here. They share the package-level harness
// and helpers (seedTenantUserMaster, contains).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/iam/audit"
)

// splitAuditDB is the per-test database with all migrations needed by
// the split-audit path applied + a live app_audit pool.
type splitAuditDB struct {
	*testpg.DB
	auditPool *pgxpool.Pool
}

// freshDBWithSplitAudit applies the full migration chain the split
// writer depends on (4→5→6→7→9→12→13→14) and returns a pool that
// connects as app_audit so tests can exercise the writer end-to-end.
func freshDBWithSplitAudit(t *testing.T) *splitAuditDB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, mig := range []struct {
		file      string
		superuser bool
	}{
		{"0004_create_tenant.up.sql", false},
		{"0005_create_users.up.sql", false},
		{"0006_create_sessions.up.sql", false},
		{"0007_create_audit_log.up.sql", false},
		{"0078_app_audit_role.up.sql", true},
		{"0083_split_audit_log.up.sql", false},
		{"0084_tenant_audit_data_retention.up.sql", false},
		{"0085_app_audit_role_split.up.sql", false},
	} {
		path := filepath.Join(harness.MigrationsDir(), mig.file)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", mig.file, err)
		}
		pool := db.AdminPool()
		if mig.superuser {
			pool = db.SuperuserPool()
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", mig.file, err)
		}
	}

	password := "test_split_audit_pw_" + uuid.New().String()[:12]
	if _, err := db.SuperuserPool().Exec(ctx, fmt.Sprintf(`ALTER ROLE app_audit WITH PASSWORD '%s'`, password)); err != nil {
		t.Fatalf("set app_audit password: %v", err)
	}

	cfg := db.SuperuserPool().Config().ConnConfig
	dsn := fmt.Sprintf("host=%s port=%d user=app_audit password=%s dbname=%s sslmode=disable",
		cfg.Host, cfg.Port, password, db.Name())
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect app_audit: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping app_audit: %v", err)
	}
	t.Cleanup(pool.Close)

	return &splitAuditDB{DB: db, auditPool: pool}
}

// seedSplitTenantUser inserts one tenant and one regular (non-master)
// user scoped to it. Returns both IDs. Distinct from seedTenantUser
// in account_lockout_test.go (which has a different signature) and
// from seedTenantUserMaster in audit_logger_test.go (which seeds a
// master).
func seedSplitTenantUser(t *testing.T, db *testpg.DB, label string) (tenantID, userID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tenantID = uuid.New()
	userID = uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantID, label, fmt.Sprintf("%s-%s.crm.local", label, tenantID)); err != nil {
		t.Fatalf("seed tenant %s: %v", label, err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role)
		 VALUES ($1, $2, $3, 'x', 'admin')`,
		userID, tenantID, fmt.Sprintf("user-%s@%s.x", userID, label)); err != nil {
		t.Fatalf("seed user %s: %v", label, err)
	}
	return tenantID, userID
}

// ---------------------------------------------------------------------------
// WriteSecurity
// ---------------------------------------------------------------------------

func TestSplitAuditLogger_WriteSecurity_InsertsRow(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	tenantID, userID := seedSplitTenantUser(t, db.DB, "sec-row")
	logger, err := postgresadapter.NewSplitAuditLogger(db.auditPool)
	if err != nil {
		t.Fatalf("NewSplitAuditLogger: %v", err)
	}
	ctx := newCtx(t)

	occurred := time.Now().UTC().Truncate(time.Microsecond)
	if err := logger.WriteSecurity(ctx, audit.SecurityAuditEvent{
		Event:       audit.SecurityEventLogin,
		ActorUserID: userID,
		TenantID:    &tenantID,
		Target:      map[string]any{"ip": "10.0.0.1"},
		OccurredAt:  occurred,
	}); err != nil {
		t.Fatalf("WriteSecurity: %v", err)
	}

	var (
		gotTenant uuid.UUID
		gotActor  uuid.UUID
		gotEvent  string
		gotTarget []byte
		gotAt     time.Time
	)
	row := db.AdminPool().QueryRow(ctx,
		`SELECT tenant_id, actor_user_id, event_type, target::text::bytea, occurred_at
		 FROM audit_log_security WHERE actor_user_id = $1`, userID)
	if err := row.Scan(&gotTenant, &gotActor, &gotEvent, &gotTarget, &gotAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotTenant != tenantID || gotActor != userID || gotEvent != "login" {
		t.Fatalf("row mismatch: tenant=%v actor=%v event=%q", gotTenant, gotActor, gotEvent)
	}
	if !gotAt.Equal(occurred) {
		t.Fatalf("occurred_at=%v, want %v", gotAt, occurred)
	}
	if !contains(string(gotTarget), "10.0.0.1") {
		t.Fatalf("target=%q, want canonical fields", string(gotTarget))
	}
}

func TestSplitAuditLogger_WriteSecurity_NullTenantForMasterEvent(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	_, masterID := seedTenantUserMaster(t, db.DB)
	logger, _ := postgresadapter.NewSplitAuditLogger(db.auditPool)
	ctx := newCtx(t)

	if err := logger.WriteSecurity(ctx, audit.SecurityAuditEvent{
		Event:       audit.SecurityEventMasterGrant,
		ActorUserID: masterID,
		TenantID:    nil, // master-context: no tenant
		Target:      map[string]any{"granted_by": "bootstrap"},
	}); err != nil {
		t.Fatalf("WriteSecurity (null tenant): %v", err)
	}

	var present bool
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT tenant_id IS NULL FROM audit_log_security WHERE actor_user_id = $1`, masterID).Scan(&present); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !present {
		t.Fatal("expected NULL tenant_id row for master event")
	}
}

func TestSplitAuditLogger_WriteSecurity_DefaultsOccurredAtWhenZero(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	tenantID, userID := seedSplitTenantUser(t, db.DB, "sec-default")
	logger, _ := postgresadapter.NewSplitAuditLogger(db.auditPool)
	ctx := newCtx(t)

	before := time.Now().UTC().Add(-time.Second)
	if err := logger.WriteSecurity(ctx, audit.SecurityAuditEvent{
		Event:       audit.SecurityEventLoginFail,
		ActorUserID: userID,
		TenantID:    &tenantID,
	}); err != nil {
		t.Fatalf("WriteSecurity: %v", err)
	}
	var got time.Time
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT occurred_at FROM audit_log_security WHERE actor_user_id = $1`, userID).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !got.After(before) {
		t.Fatalf("occurred_at=%v, want > %v", got, before)
	}
}

func TestSplitAuditLogger_WriteSecurity_RejectsInvalid(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	tenantID, userID := seedSplitTenantUser(t, db.DB, "sec-invalid")
	logger, _ := postgresadapter.NewSplitAuditLogger(db.auditPool)
	ctx := newCtx(t)

	cases := []struct {
		name string
		ev   audit.SecurityAuditEvent
	}{
		{"unknown event", audit.SecurityAuditEvent{Event: "not_a_real_event", ActorUserID: userID, TenantID: &tenantID}},
		{"empty event", audit.SecurityAuditEvent{Event: "", ActorUserID: userID, TenantID: &tenantID}},
		{"zero actor", audit.SecurityAuditEvent{Event: audit.SecurityEventLogin, TenantID: &tenantID}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := logger.WriteSecurity(ctx, tc.ev)
			if !errors.Is(err, postgresadapter.ErrSplitAuditEventInvalid) {
				t.Fatalf("err=%v, want ErrSplitAuditEventInvalid", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// WriteData
// ---------------------------------------------------------------------------

func TestSplitAuditLogger_WriteData_InsertsRow(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	tenantID, userID := seedSplitTenantUser(t, db.DB, "data-row")
	logger, _ := postgresadapter.NewSplitAuditLogger(db.auditPool)
	ctx := newCtx(t)

	occurred := time.Now().UTC().Truncate(time.Microsecond)
	if err := logger.WriteData(ctx, audit.DataAuditEvent{
		Event:       audit.DataEventReadPII,
		ActorUserID: userID,
		TenantID:    tenantID,
		Target:      map[string]any{"record_kind": "associate", "record_id": "42"},
		OccurredAt:  occurred,
	}); err != nil {
		t.Fatalf("WriteData: %v", err)
	}

	var (
		gotTenant uuid.UUID
		gotActor  uuid.UUID
		gotEvent  string
		gotAt     time.Time
	)
	row := db.AdminPool().QueryRow(ctx,
		`SELECT tenant_id, actor_user_id, event_type, occurred_at
		 FROM audit_log_data WHERE actor_user_id = $1`, userID)
	if err := row.Scan(&gotTenant, &gotActor, &gotEvent, &gotAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotTenant != tenantID || gotActor != userID || gotEvent != "read_pii" {
		t.Fatalf("row mismatch: tenant=%v actor=%v event=%q", gotTenant, gotActor, gotEvent)
	}
	if !gotAt.Equal(occurred) {
		t.Fatalf("occurred_at=%v, want %v", gotAt, occurred)
	}
}

func TestSplitAuditLogger_WriteData_RejectsInvalid(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	tenantID, userID := seedSplitTenantUser(t, db.DB, "data-invalid")
	logger, _ := postgresadapter.NewSplitAuditLogger(db.auditPool)
	ctx := newCtx(t)

	cases := []struct {
		name string
		ev   audit.DataAuditEvent
	}{
		{"unknown event", audit.DataAuditEvent{Event: "not_a_real_event", ActorUserID: userID, TenantID: tenantID}},
		{"empty event", audit.DataAuditEvent{Event: "", ActorUserID: userID, TenantID: tenantID}},
		{"zero actor", audit.DataAuditEvent{Event: audit.DataEventReadPII, TenantID: tenantID}},
		{"zero tenant", audit.DataAuditEvent{Event: audit.DataEventReadPII, ActorUserID: userID}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := logger.WriteData(ctx, tc.ev)
			if !errors.Is(err, postgresadapter.ErrSplitAuditEventInvalid) {
				t.Fatalf("err=%v, want ErrSplitAuditEventInvalid", err)
			}
		})
	}
}

func TestSplitAuditLogger_NewWithNilPool(t *testing.T) {
	t.Parallel()
	if _, err := postgresadapter.NewSplitAuditLogger(nil); !errors.Is(err, postgresadapter.ErrNilPool) {
		t.Fatalf("err=%v, want ErrNilPool", err)
	}
}

// ---------------------------------------------------------------------------
// Migration shape: tenants column, app_audit grants, view, RLS
// ---------------------------------------------------------------------------

func TestSplitAuditMigration_TenantsHasRetentionColumn(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	ctx := newCtx(t)
	tenantID, _ := seedSplitTenantUser(t, db.DB, "retention")
	var got int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT audit_data_retention_months FROM tenants WHERE id = $1`, tenantID).Scan(&got); err != nil {
		t.Fatalf("read retention column: %v", err)
	}
	if got != 12 {
		t.Fatalf("audit_data_retention_months default=%d, want 12", got)
	}

	// Override path: master_ops can update it within bounds.
	if _, err := db.AdminPool().Exec(ctx,
		`UPDATE tenants SET audit_data_retention_months = 36 WHERE id = $1`, tenantID); err != nil {
		t.Fatalf("update retention: %v", err)
	}
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT audit_data_retention_months FROM tenants WHERE id = $1`, tenantID).Scan(&got); err != nil {
		t.Fatalf("re-read retention: %v", err)
	}
	if got != 36 {
		t.Fatalf("retention after update=%d, want 36", got)
	}

	// Bound enforcement: 0 is rejected.
	if _, err := db.AdminPool().Exec(ctx,
		`UPDATE tenants SET audit_data_retention_months = 0 WHERE id = $1`, tenantID); err == nil {
		t.Fatal("expected CHECK violation for retention=0")
	}
}

func TestSplitAuditMigration_AppAuditCanInsertNewTablesOnly(t *testing.T) {
	// AC: app_audit can INSERT into the split tables and nothing else.
	// This mirrors TestAppAuditRole_PrivilegesAreLeastPrivilege from
	// audit_logger_test.go but for the new tables granted in 0014.
	db := freshDBWithSplitAudit(t)
	tenantID, userID := seedSplitTenantUser(t, db.DB, "least-priv-split")
	ctx := newCtx(t)

	// INSERT security: must succeed.
	if _, err := db.auditPool.Exec(ctx,
		`INSERT INTO audit_log_security (tenant_id, actor_user_id, event_type, target)
		 VALUES ($1, $2, 'login', '{}'::jsonb)`,
		tenantID, userID); err != nil {
		t.Fatalf("INSERT audit_log_security: %v", err)
	}
	// INSERT data: must succeed.
	if _, err := db.auditPool.Exec(ctx,
		`INSERT INTO audit_log_data (tenant_id, actor_user_id, event_type, target)
		 VALUES ($1, $2, 'read_pii', '{}'::jsonb)`,
		tenantID, userID); err != nil {
		t.Fatalf("INSERT audit_log_data: %v", err)
	}
	// SELECT split tables: must be denied.
	if err := db.auditPool.QueryRow(ctx, `SELECT count(*) FROM audit_log_security`).Scan(new(int)); err == nil {
		t.Fatal("SELECT audit_log_security unexpectedly allowed")
	}
	if err := db.auditPool.QueryRow(ctx, `SELECT count(*) FROM audit_log_data`).Scan(new(int)); err == nil {
		t.Fatal("SELECT audit_log_data unexpectedly allowed")
	}
	// UPDATE/DELETE: must be denied.
	if _, err := db.auditPool.Exec(ctx, `UPDATE audit_log_security SET event_type = 'tampered'`); err == nil {
		t.Fatal("UPDATE audit_log_security unexpectedly allowed")
	}
	if _, err := db.auditPool.Exec(ctx, `DELETE FROM audit_log_data`); err == nil {
		t.Fatal("DELETE audit_log_data unexpectedly allowed")
	}
}

func TestSplitAuditMigration_UnifiedViewReturnsBoth(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	tenantID, userID := seedSplitTenantUser(t, db.DB, "unified-view")
	logger, _ := postgresadapter.NewSplitAuditLogger(db.auditPool)
	ctx := newCtx(t)

	if err := logger.WriteSecurity(ctx, audit.SecurityAuditEvent{
		Event: audit.SecurityEventLogin, ActorUserID: userID, TenantID: &tenantID,
	}); err != nil {
		t.Fatalf("WriteSecurity: %v", err)
	}
	if err := logger.WriteData(ctx, audit.DataAuditEvent{
		Event: audit.DataEventExportCSV, ActorUserID: userID, TenantID: tenantID,
	}); err != nil {
		t.Fatalf("WriteData: %v", err)
	}

	rows, err := db.AdminPool().Query(ctx,
		`SELECT source, event_type FROM audit_log_unified WHERE actor_user_id = $1 ORDER BY source`, userID)
	if err != nil {
		t.Fatalf("query view: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var src, ev string
		if err := rows.Scan(&src, &ev); err != nil {
			t.Fatalf("scan view: %v", err)
		}
		got[src] = ev
	}
	if rows.Err() != nil {
		t.Fatalf("iter view: %v", rows.Err())
	}
	if got["security"] != "login" || got["data"] != "export_csv" {
		t.Fatalf("unified view rows=%v, want security=login + data=export_csv", got)
	}
}

func TestSplitAuditMigration_UpDownUp(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	ctx := newCtx(t)

	// up has been applied. Confirm both tables exist, then down, then up again.
	if !tableExists(t, ctx, db.DB, "audit_log_security") || !tableExists(t, ctx, db.DB, "audit_log_data") {
		t.Fatal("split tables missing after initial up")
	}

	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0083_split_audit_log.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if tableExists(t, ctx, db.DB, "audit_log_security") || tableExists(t, ctx, db.DB, "audit_log_data") {
		t.Fatal("split tables still present after down")
	}
	// Down twice is idempotent.
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down again: %v", err)
	}

	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0083_split_audit_log.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if !tableExists(t, ctx, db.DB, "audit_log_security") || !tableExists(t, ctx, db.DB, "audit_log_data") {
		t.Fatal("split tables missing after re-up")
	}
}

func TestSplitAuditMigration_RetentionUpDownUp(t *testing.T) {
	db := freshDBWithSplitAudit(t)
	ctx := newCtx(t)

	if !columnExists(t, ctx, db.DB, "tenants", "audit_data_retention_months") {
		t.Fatal("retention column missing after initial up")
	}

	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0084_tenant_audit_data_retention.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if columnExists(t, ctx, db.DB, "tenants", "audit_data_retention_months") {
		t.Fatal("retention column still present after down")
	}
	// Down twice is idempotent.
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down again: %v", err)
	}

	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0084_tenant_audit_data_retention.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if !columnExists(t, ctx, db.DB, "tenants", "audit_data_retention_months") {
		t.Fatal("retention column missing after re-up")
	}
}

// ---------------------------------------------------------------------------
// helpers (file-local; package-shared helpers live in audit_logger_test.go
// and withtenant_test.go)
// ---------------------------------------------------------------------------

func tableExists(t *testing.T, ctx context.Context, db *testpg.DB, name string) bool {
	t.Helper()
	var got bool
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		   WHERE c.relname = $1 AND c.relkind = 'r' AND n.nspname = 'public'
		 )`, name).Scan(&got); err != nil {
		t.Fatalf("table probe: %v", err)
	}
	return got
}

func columnExists(t *testing.T, ctx context.Context, db *testpg.DB, table, column string) bool {
	t.Helper()
	var got bool
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM information_schema.columns
		   WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
		 )`, table, column).Scan(&got); err != nil {
		t.Fatalf("column probe: %v", err)
	}
	return got
}
