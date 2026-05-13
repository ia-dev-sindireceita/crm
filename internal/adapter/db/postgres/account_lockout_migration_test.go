package postgres_test

// SIN-62341 acceptance criterion #9: 0008_account_lockout migrates up
// and down cleanly. The down path is idempotent (DROP TABLE IF EXISTS)
// and the up/down/up cycle MUST leave the table in a usable state.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

func TestAccountLockoutMigration_UpDownUp(t *testing.T) {
	db := freshDBWithLockout(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if !lockoutTableExists(t, ctx, db) {
		t.Fatal("account_lockout missing after initial up")
	}

	// Apply the down migration.
	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0008_account_lockout.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if lockoutTableExists(t, ctx, db) {
		t.Fatal("account_lockout still present after down")
	}

	// Re-apply the up migration; the IF NOT EXISTS guards make it
	// idempotent so re-application is safe.
	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0008_account_lockout.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if !lockoutTableExists(t, ctx, db) {
		t.Fatal("account_lockout missing after re-up")
	}

	// Down twice is a noop (DROP IF EXISTS).
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down (idempotent): %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down again: %v", err)
	}
}

func lockoutTableExists(t *testing.T, ctx context.Context, db *testpg.DB) bool {
	t.Helper()
	var exists bool
	row := db.SuperuserPool().QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relname = 'account_lockout' AND n.nspname = 'public'
		)`)
	if err := row.Scan(&exists); err != nil {
		t.Fatalf("table-exists probe: %v", err)
	}
	return exists
}
