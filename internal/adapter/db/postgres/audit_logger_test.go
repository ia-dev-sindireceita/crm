package postgres_test

// SIN-62219: integration tests for the postgres audit_logger adapter
// and the 0009_app_audit_role migration.
//
// Tests apply migrations 0004 (tenants) → 0005 (users) → 0007
// (audit_log) → 0009 (app_audit role) on top of the harness's default
// 0001-0003 sequence, then exercise the writer through the dedicated
// app_audit pool. The audit pool's privileges (INSERT-only on
// audit_log, BYPASSRLS) are themselves under test — see
// TestAppAuditRole_PrivilegesAreLeastPrivilege.

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
	"github.com/pericles-luz/crm/internal/tenancy"
)

// auditDB is the per-test database with all migrations needed by the
// audit-log path applied + a live app_audit pool.
type auditDB struct {
	*testpg.DB
	auditPool *pgxpool.Pool
}

// freshDBWithAudit applies the migration chain audit_log depends on
// (4→5→7→9), sets a password for app_audit, and returns a pool that
// connects as app_audit so tests can exercise the writer end-to-end.
func freshDBWithAudit(t *testing.T) *auditDB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Migrations 0004-0007 are app-object DDL; run as app_admin.
	// Migration 0009 creates a cluster-level role and must run as
	// the cluster superuser, matching the operational posture of
	// 0001_roles.up.sql (see docs/adr/0071-postgres-roles.md
	// "Credential injection").
	for _, mig := range []struct {
		file      string
		superuser bool
	}{
		{"0004_create_tenant.up.sql", false},
		{"0005_create_users.up.sql", false},
		{"0006_create_sessions.up.sql", false},
		{"0007_create_audit_log.up.sql", false},
		{"0009_app_audit_role.up.sql", true},
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

	password := "test_audit_pw_" + uuid.New().String()[:12]
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

	return &auditDB{DB: db, auditPool: pool}
}

// seedTenantUserMaster inserts a tenant and a master user. Returns
// the tenant id and the master user id. The master user is required
// because audit_log.actor_user_id has a FK to users(id).
func seedTenantUserMaster(t *testing.T, db *testpg.DB) (tenantID, masterID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tenantID = uuid.New()
	masterID = uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantID, "audit-target", fmt.Sprintf("audit-%s.crm.local", tenantID)); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, NULL, $2, 'x', 'master', true)`,
		masterID, fmt.Sprintf("master-%s@x", masterID)); err != nil {
		t.Fatalf("seed master: %v", err)
	}
	return tenantID, masterID
}

func TestAuditLogger_InsertsRow(t *testing.T) {
	db := freshDBWithAudit(t)
	tenantID, masterID := seedTenantUserMaster(t, db.DB)

	logger, err := postgresadapter.NewAuditLogger(db.auditPool)
	if err != nil {
		t.Fatalf("NewAuditLogger: %v", err)
	}
	ctx := newCtx(t)

	startedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := logger.Log(ctx, audit.AuditEvent{
		Event:       audit.EventImpersonationStarted,
		ActorUserID: masterID,
		TenantID:    &tenantID,
		Target: map[string]any{
			"tenant_id": tenantID.String(),
			"reason":    "incident-INC-7",
		},
		CreatedAt: startedAt,
	}); err != nil {
		t.Fatalf("Log started: %v", err)
	}

	var (
		gotTenant uuid.UUID
		gotActor  uuid.UUID
		gotEvent  string
		gotTarget []byte
		gotAt     time.Time
	)
	row := db.AdminPool().QueryRow(ctx,
		`SELECT tenant_id, actor_user_id, event, target::text::bytea, created_at
		 FROM audit_log WHERE actor_user_id = $1`, masterID)
	if err := row.Scan(&gotTenant, &gotActor, &gotEvent, &gotTarget, &gotAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotTenant != tenantID {
		t.Fatalf("tenant_id=%v, want %v", gotTenant, tenantID)
	}
	if gotActor != masterID {
		t.Fatalf("actor_user_id=%v, want %v", gotActor, masterID)
	}
	if gotEvent != audit.EventImpersonationStarted {
		t.Fatalf("event=%q, want %q", gotEvent, audit.EventImpersonationStarted)
	}
	if !gotAt.Equal(startedAt) {
		t.Fatalf("created_at=%v, want %v", gotAt, startedAt)
	}
	// Target round-trip: just confirm the canonical fields landed.
	if got := string(gotTarget); !contains(got, "incident-INC-7") || !contains(got, tenantID.String()) {
		t.Fatalf("target=%q, want canonical fields", got)
	}
}

func TestAuditLogger_DefaultsCreatedAtWhenZero(t *testing.T) {
	db := freshDBWithAudit(t)
	tenantID, masterID := seedTenantUserMaster(t, db.DB)
	logger, _ := postgresadapter.NewAuditLogger(db.auditPool)
	ctx := newCtx(t)

	before := time.Now().UTC().Add(-time.Second)
	if err := logger.Log(ctx, audit.AuditEvent{
		Event:       audit.EventImpersonationEnded,
		ActorUserID: masterID,
		TenantID:    &tenantID,
		// CreatedAt deliberately zero — the column DEFAULT now()
		// must take over.
	}); err != nil {
		t.Fatalf("Log: %v", err)
	}
	var got time.Time
	row := db.AdminPool().QueryRow(ctx,
		`SELECT created_at FROM audit_log WHERE actor_user_id = $1`, masterID)
	if err := row.Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !got.After(before) {
		t.Fatalf("created_at=%v, want > %v", got, before)
	}
}

func TestAuditLogger_RejectsInvalidEvents(t *testing.T) {
	t.Parallel()
	db := freshDBWithAudit(t)
	tenantID, masterID := seedTenantUserMaster(t, db.DB)
	logger, _ := postgresadapter.NewAuditLogger(db.auditPool)
	ctx := newCtx(t)

	cases := []struct {
		name  string
		event audit.AuditEvent
	}{
		{"empty event", audit.AuditEvent{ActorUserID: masterID, TenantID: &tenantID}},
		{"zero actor", audit.AuditEvent{Event: "x", TenantID: &tenantID}},
		{"nil tenant", audit.AuditEvent{Event: "x", ActorUserID: masterID, TenantID: nil}},
		{"zero tenant", audit.AuditEvent{Event: "x", ActorUserID: masterID, TenantID: zeroUUIDPointer()}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := logger.Log(ctx, tc.event)
			if !errors.Is(err, postgresadapter.ErrAuditEventInvalid) {
				t.Fatalf("err=%v, want ErrAuditEventInvalid", err)
			}
		})
	}
}

func TestAuditLogger_NewWithNilPool(t *testing.T) {
	t.Parallel()
	if _, err := postgresadapter.NewAuditLogger(nil); !errors.Is(err, postgresadapter.ErrNilPool) {
		t.Fatalf("err=%v, want ErrNilPool", err)
	}
}

func TestResolveByID_KnownAndUnknown(t *testing.T) {
	db := harness.DB(t)
	applyTenantMigration(t, db)
	seedAcmeAndGlobex(t, db)
	resolver, _ := postgresadapter.NewTenantResolver(db.RuntimePool())
	ctx := newCtx(t)

	got, err := resolver.ResolveByID(ctx, uuid.MustParse(acmeID))
	if err != nil {
		t.Fatalf("known id: %v", err)
	}
	if got.Host != "acme.crm.local" {
		t.Fatalf("got.Host=%q, want acme.crm.local", got.Host)
	}

	if _, err := resolver.ResolveByID(ctx, uuid.New()); !errors.Is(err, tenancy.ErrTenantNotFound) {
		t.Fatalf("unknown id: err=%v, want ErrTenantNotFound", err)
	}
	if _, err := resolver.ResolveByID(ctx, uuid.Nil); !errors.Is(err, tenancy.ErrTenantNotFound) {
		t.Fatalf("nil id: err=%v, want ErrTenantNotFound", err)
	}
}

func TestAppAuditRole_PrivilegesAreLeastPrivilege(t *testing.T) {
	// AC: app_audit can INSERT into audit_log and nothing else. Probe
	// every other audit_log mutation + an unrelated table to prove
	// the grant is correctly scoped.
	db := freshDBWithAudit(t)
	tenantID, masterID := seedTenantUserMaster(t, db.DB)
	ctx := newCtx(t)

	// INSERT: must succeed.
	if _, err := db.auditPool.Exec(ctx,
		`INSERT INTO audit_log (tenant_id, actor_user_id, event, target)
		 VALUES ($1, $2, 'least-priv-test', '{}'::jsonb)`,
		tenantID, masterID); err != nil {
		t.Fatalf("INSERT audit_log: %v", err)
	}
	// SELECT: must be denied.
	var n int
	if err := db.auditPool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&n); err == nil {
		t.Fatalf("SELECT audit_log unexpectedly allowed (n=%d)", n)
	}
	// UPDATE: must be denied.
	if _, err := db.auditPool.Exec(ctx, `UPDATE audit_log SET event = 'tampered'`); err == nil {
		t.Fatal("UPDATE audit_log unexpectedly allowed")
	}
	// DELETE: must be denied.
	if _, err := db.auditPool.Exec(ctx, `DELETE FROM audit_log`); err == nil {
		t.Fatal("DELETE audit_log unexpectedly allowed")
	}
	// Unrelated table: must be denied.
	if _, err := db.auditPool.Exec(ctx, `SELECT 1 FROM users LIMIT 1`); err == nil {
		t.Fatal("SELECT users unexpectedly allowed")
	}
}

func TestAppAuditRoleMigration_UpDownUp(t *testing.T) {
	db := freshDBWithAudit(t)
	ctx := newCtx(t)

	// up has already been applied by freshDBWithAudit. Confirm the
	// role exists, then run the down migration, then re-apply up.
	if !roleExists(t, ctx, db.DB, "app_audit") {
		t.Fatal("app_audit missing after initial up")
	}

	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0009_app_audit_role.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	// down requires superuser (DROP ROLE / DROP OWNED). Use the
	// superuser pool so the test exercises the same role the
	// production migration runner would.
	if _, err := db.SuperuserPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if roleExists(t, ctx, db.DB, "app_audit") {
		t.Fatal("app_audit still present after down")
	}

	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0009_app_audit_role.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	// up creates a role and grants on audit_log; the role-creation
	// branch needs CREATEROLE so we run it as superuser.
	if _, err := db.SuperuserPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if !roleExists(t, ctx, db.DB, "app_audit") {
		t.Fatal("app_audit missing after re-up")
	}
	// Down twice is idempotent.
	if _, err := db.SuperuserPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down (idempotent): %v", err)
	}
	if _, err := db.SuperuserPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down again: %v", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func zeroUUIDPointer() *uuid.UUID {
	id := uuid.Nil
	return &id
}

func roleExists(t *testing.T, ctx context.Context, db *testpg.DB, name string) bool {
	t.Helper()
	var got bool
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, name).Scan(&got); err != nil {
		t.Fatalf("role probe: %v", err)
	}
	return got
}
