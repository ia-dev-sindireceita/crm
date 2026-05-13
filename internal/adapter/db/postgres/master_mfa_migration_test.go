package postgres_test

// SIN-62342 acceptance criterion §schema: 0009_master_mfa migrates up
// and down cleanly. Mirrors the SIN-62341 0008 pattern — a single
// up/down/up cycle proves both directions are idempotent and round-
// trip safe.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

func TestMasterMFAMigration_UpDownUp(t *testing.T) {
	db := freshDBWithMasterMFA(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if !mfaTablesExist(t, ctx, db) {
		t.Fatal("master_mfa or master_recovery_code missing after initial up")
	}

	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0009_master_mfa.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if mfaTablesExist(t, ctx, db) {
		t.Fatal("master_mfa or master_recovery_code still present after down")
	}

	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0009_master_mfa.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if !mfaTablesExist(t, ctx, db) {
		t.Fatal("master_mfa or master_recovery_code missing after re-up")
	}

	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down (idempotent): %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down again: %v", err)
	}
}

// freshDBWithMasterMFA brings up a per-test DB with all migrations the
// master_mfa adapter depends on: 0004 (tenants), 0005 (users), 0008
// (account_lockout — used by other tests sharing the DB) and 0009
// itself. 0002 (master_ops_audit) is already applied by the harness.
func freshDBWithMasterMFA(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0009_master_mfa.up.sql",
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

func mfaTablesExist(t *testing.T, ctx context.Context, db *testpg.DB) bool {
	t.Helper()
	var count int
	row := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relname IN ('master_mfa', 'master_recovery_code')
		    AND n.nspname = 'public'`)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("table-exists probe: %v", err)
	}
	return count == 2
}
