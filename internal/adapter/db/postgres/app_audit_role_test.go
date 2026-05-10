package postgres_test

// SIN-62424 (Phase B.2): the up/down/up cycle for the dedicated
// app_audit role (migration 0009) used to live in audit_logger_test.go.
// When that file was deleted alongside the legacy AuditLogger, this
// test was MOVED here (not removed) — the role itself survives the
// retirement of the legacy audit_log table because 0014 wired its
// least-privileged INSERT grants onto audit_log_security and
// audit_log_data, so migration 0009's lifecycle still needs to be
// exercised.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// freshDBWithAppAuditRole applies the migrations needed to bring the
// app_audit role into existence (4→5→6→7→9) on top of the harness's
// default 0001-0003 sequence and returns the db plus a live app_audit
// pool. Phase B.2's drop-audit-log migration (0015) is intentionally
// NOT applied here: this test asserts migration 0009 in isolation.
func freshDBWithAppAuditRole(t *testing.T) *splitAuditDB {
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

	password := "test_app_audit_pw_" + uuid.New().String()[:12]
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

// TestAppAuditRoleMigration_UpDownUp asserts that the role-lifecycle
// migration 0009 is reversible: up creates the role, down drops it,
// re-up brings it back, and down is idempotent.
func TestAppAuditRoleMigration_UpDownUp(t *testing.T) {
	db := freshDBWithAppAuditRole(t)
	ctx := newCtx(t)

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
