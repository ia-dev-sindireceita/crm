package postgres_test

// SIN-66566 (parent SIN-66536): dedicated read-only backup role for pg_dump.
//
// These assertions run against a REAL Postgres (the package testpg harness — no
// DB mocking, rule 5). CREATE ROLE is cluster-global, so we drive the migration
// on a superuser connection to the per-test database and inspect pg_roles /
// pg_has_role, which are cluster-scoped catalogs.
//
// What this proves (AC #3 of SIN-66566):
//   - migrations/0133_app_backup_role.up.sql applies cleanly and creates
//     `app_backup` with rolbypassrls=t (reads through every RLS policy),
//     rolsuper=f, rolcreatedb=f, rolcreaterole=f, rolcanlogin=t.
//   - `app_backup` is a member of the predefined pg_read_all_data role (SELECT
//     on every table without per-table GRANT maintenance).
//   - the up migration is idempotent (re-apply is a no-op via the DO guard +
//     idempotent GRANT).
//   - the down migration revokes membership and drops the role cleanly.
//
// These tests are intentionally NOT parallel: they mutate cluster-global role
// state, and Go runs non-parallel tests serially before the parallel phase, so
// they never race each other.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// applyBackupRoleMigration executes a 0133_app_backup_role migration file
// (up or down) as the cluster superuser against pool.
func applyBackupRoleMigration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, file string) {
	t.Helper()
	path := filepath.Join(harness.MigrationsDir(), file)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply %s: %v", file, err)
	}
}

// backupRoleExists reports whether the cluster has an app_backup role.
func backupRoleExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = 'app_backup')`).
		Scan(&exists); err != nil {
		t.Fatalf("probe app_backup existence: %v", err)
	}
	return exists
}

func TestAppBackupRoleIsReadOnlyBypassRLS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db := harness.DB(t)
	su := db.SuperuserPool()
	applyBackupRoleMigration(t, ctx, su, "0133_app_backup_role.up.sql")

	var canLogin, super, createRole, createDB, bypassRLS bool
	if err := su.QueryRow(ctx,
		`SELECT rolcanlogin, rolsuper, rolcreaterole, rolcreatedb, rolbypassrls
		   FROM pg_roles WHERE rolname = 'app_backup'`).
		Scan(&canLogin, &super, &createRole, &createDB, &bypassRLS); err != nil {
		t.Fatalf("read pg_roles for app_backup: %v", err)
	}

	if !canLogin {
		t.Error("app_backup: rolcanlogin = false, want true (LOGIN — the sidecar authenticates as it)")
	}
	if super {
		t.Error("app_backup: rolsuper = true, want false (NOSUPERUSER)")
	}
	if createRole {
		t.Error("app_backup: rolcreaterole = true, want false (NOCREATEROLE)")
	}
	if createDB {
		t.Error("app_backup: rolcreatedb = true, want false (NOCREATEDB)")
	}
	if !bypassRLS {
		t.Error("app_backup: rolbypassrls = false, want true (pg_dump must read through the 191 RLS policies)")
	}

	// Membership in the predefined pg_read_all_data role = SELECT on everything.
	var readsAll bool
	if err := su.QueryRow(ctx,
		`SELECT pg_has_role('app_backup', 'pg_read_all_data', 'MEMBER')`).
		Scan(&readsAll); err != nil {
		t.Fatalf("probe pg_read_all_data membership: %v", err)
	}
	if !readsAll {
		t.Error("app_backup is not a member of pg_read_all_data; pg_dump would miss tables it lacks explicit SELECT on")
	}
}

// TestAppBackupRoleMigrationIsIdempotent re-applies the up migration and
// expects no error (DO-block guard + idempotent GRANT), matching the
// 0001_roles.up.sql convention.
func TestAppBackupRoleMigrationIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db := harness.DB(t)
	su := db.SuperuserPool()
	applyBackupRoleMigration(t, ctx, su, "0133_app_backup_role.up.sql")
	applyBackupRoleMigration(t, ctx, su, "0133_app_backup_role.up.sql")

	if !backupRoleExists(t, ctx, su) {
		t.Fatal("app_backup missing after a second up-migration apply")
	}
}

// TestAppBackupRoleDownDropsRole proves the down migration reverses the up:
// membership is revoked and the role is dropped. It re-applies the up at the
// end so the cluster is left in the provisioned state (defensive; no other
// test depends on the role either way).
func TestAppBackupRoleDownDropsRole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db := harness.DB(t)
	su := db.SuperuserPool()

	applyBackupRoleMigration(t, ctx, su, "0133_app_backup_role.up.sql")
	if !backupRoleExists(t, ctx, su) {
		t.Fatal("app_backup missing after up-migration; cannot test down")
	}

	applyBackupRoleMigration(t, ctx, su, "0133_app_backup_role.down.sql")
	if backupRoleExists(t, ctx, su) {
		t.Fatal("app_backup still present after down-migration; DROP ROLE did not run")
	}

	// Down must be idempotent too (guarded by the IF EXISTS in the DO block).
	applyBackupRoleMigration(t, ctx, su, "0133_app_backup_role.down.sql")

	// Restore provisioned state for any later test/run on the shared cluster.
	applyBackupRoleMigration(t, ctx, su, "0133_app_backup_role.up.sql")
}
