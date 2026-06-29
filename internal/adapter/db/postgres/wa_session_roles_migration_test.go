package postgres_test

// SIN-66298 (R1.2 of SIN-66291 / ADR-0108): least-privilege roles for the
// dedicated WhatsApp-session (whatsmeow) credential database.
//
// These assertions run against a REAL Postgres (the package testpg harness —
// no DB mocking, rule 5). The harness gives a fresh database per test; we treat
// it as a stand-in for the dedicated WA session DB and apply
// migrations/wa_session/0001_wa_session_roles.up.sql to it. The role/grant
// model is database-agnostic — the migration only ever touches the dedicated
// roles and the whatsmeow_* tables — so exercising it here is faithful.
//
// We drive each role with SET ROLE on a superuser connection. Per Postgres,
// when a superuser SET ROLEs to a non-superuser role it LOSES its superuser
// privileges for permission checks, so the grant/deny behaviour observed is
// exactly what that role would see authenticating directly.
//
// What this proves (the ACs):
//   - wa_session_runtime can SELECT/INSERT/UPDATE/DELETE on whatsmeow_* …
//   - … but CANNOT DROP/ALTER/CREATE (no DDL) — 42501 insufficient_privilege.
//   - wa_session_admin CAN run DDL (the Upgrade deploy step).
//   - ALTER DEFAULT PRIVILEGES auto-grants DML to runtime for tables the admin
//     creates AFTER the migration (the first-deploy path).
//   - the grant loop covers whatsmeow_* tables that already exist (re-deploy).
//   - app_runtime has NO access to the session credential store.
//   - the read-only queries whatsmeow's boot Upgrade issues on a CURRENT schema
//     succeed under wa_session_runtime (boot is a DDL-free no-op), while the
//     DDL path that a stale schema would require is denied (must run as admin).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// applyWASessionRoles loads and executes the role migration as the cluster
// superuser against db. CREATE ROLE is cluster-global and idempotent, so
// re-running across tests in the shared cluster is safe.
func applyWASessionRoles(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	path := filepath.Join(harness.MigrationsDir(), "wa_session", "0001_wa_session_roles.up.sql")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply wa_session roles migration: %v", err)
	}
}

// expectPrivDenied asserts err is a Postgres 42501 (insufficient_privilege).
func expectPrivDenied(t *testing.T, err error, op string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: want 42501 insufficient_privilege, got nil (operation allowed)", op)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: want PgError, got %T %v", op, err, err)
	}
	if pgErr.Code != "42501" {
		t.Fatalf("%s: want code 42501, got %s (%s)", op, pgErr.Code, pgErr.Message)
	}
}

// expectNoAccess asserts the operation was denied. A role with no USAGE on the
// schema cannot even resolve a table name, so Postgres raises 42P01
// (undefined_table) rather than 42501 (insufficient_privilege). Both prove the
// role has no reach into the object — and 42P01 is the stronger outcome (it
// leaks no existence oracle). Either is acceptable for "this role has no
// access at all".
func expectNoAccess(t *testing.T, err error, op string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: want access denied, got nil (operation allowed)", op)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: want PgError, got %T %v", op, err, err)
	}
	if pgErr.Code != "42501" && pgErr.Code != "42P01" {
		t.Fatalf("%s: want code 42501 or 42P01 (denied), got %s (%s)", op, pgErr.Code, pgErr.Message)
	}
}

func TestWASessionRolesLeastPrivilege(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db := harness.DB(t)
	su := db.SuperuserPool()

	// Pre-existing whatsmeow_* table created BEFORE the migration runs — stands
	// in for a re-deploy where the schema already exists. The migration's grant
	// loop must hand runtime DML on it.
	if _, err := su.Exec(ctx,
		`CREATE TABLE whatsmeow_preexisting (jid text PRIMARY KEY, note text)`); err != nil {
		t.Fatalf("create pre-existing whatsmeow table: %v", err)
	}

	applyWASessionRoles(t, ctx, su)

	// A single connection we flip between roles via SET ROLE.
	conn, err := su.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire superuser conn: %v", err)
	}
	defer conn.Release()

	setRole := func(role string) {
		t.Helper()
		if _, err := conn.Exec(ctx, "SET ROLE "+role); err != nil {
			t.Fatalf("SET ROLE %s: %v", role, err)
		}
	}
	resetRole := func() {
		if _, err := conn.Exec(ctx, "RESET ROLE"); err != nil {
			t.Fatalf("RESET ROLE: %v", err)
		}
	}

	// --- admin runs the schema DDL (the Upgrade deploy step) -----------------
	// Creating as wa_session_admin means the table is owned by admin AND falls
	// under ALTER DEFAULT PRIVILEGES FOR ROLE wa_session_admin → runtime gets
	// DML automatically, with no second grant pass.
	setRole("wa_session_admin")
	if _, err := conn.Exec(ctx,
		`CREATE TABLE whatsmeow_device (jid text PRIMARY KEY, noise_key bytea)`); err != nil {
		t.Fatalf("admin CREATE whatsmeow_device (admin must run DDL): %v", err)
	}
	// whatsmeow_version + a row: lets us replay the boot Upgrade's read path.
	if _, err := conn.Exec(ctx,
		`CREATE TABLE whatsmeow_version (version INTEGER, compat INTEGER)`); err != nil {
		t.Fatalf("admin CREATE whatsmeow_version: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO whatsmeow_version (version, compat) VALUES (14, 1)`); err != nil {
		t.Fatalf("admin seed whatsmeow_version: %v", err)
	}
	resetRole()

	// --- runtime: DML allowed on whatsmeow_* (default-priv + grant-loop) ------
	setRole("wa_session_runtime")

	// whatsmeow_device — covered by ALTER DEFAULT PRIVILEGES (created after).
	if _, err := conn.Exec(ctx,
		`INSERT INTO whatsmeow_device (jid, noise_key) VALUES ('5511999999999', '\x00')`); err != nil {
		t.Fatalf("runtime INSERT whatsmeow_device: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`UPDATE whatsmeow_device SET noise_key = '\x01' WHERE jid = '5511999999999'`); err != nil {
		t.Fatalf("runtime UPDATE whatsmeow_device: %v", err)
	}
	var jid string
	if err := conn.QueryRow(ctx,
		`SELECT jid FROM whatsmeow_device LIMIT 1`).Scan(&jid); err != nil {
		t.Fatalf("runtime SELECT whatsmeow_device: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`DELETE FROM whatsmeow_device WHERE jid = '5511999999999'`); err != nil {
		t.Fatalf("runtime DELETE whatsmeow_device: %v", err)
	}

	// whatsmeow_preexisting — covered by the migration's grant loop.
	if _, err := conn.Exec(ctx,
		`INSERT INTO whatsmeow_preexisting (jid, note) VALUES ('x', 'y')`); err != nil {
		t.Fatalf("runtime INSERT whatsmeow_preexisting (grant loop): %v", err)
	}

	// --- runtime: DDL denied (no CREATE/ALTER/DROP) --------------------------
	_, err = conn.Exec(ctx, `DROP TABLE whatsmeow_device`)
	expectPrivDenied(t, err, "runtime DROP whatsmeow_device")

	_, err = conn.Exec(ctx, `ALTER TABLE whatsmeow_device ADD COLUMN sneaky text`)
	expectPrivDenied(t, err, "runtime ALTER whatsmeow_device")

	_, err = conn.Exec(ctx, `CREATE TABLE whatsmeow_runtime_made (id int)`)
	expectPrivDenied(t, err, "runtime CREATE table")

	// --- runtime: a CURRENT-schema boot Upgrade is a read-only no-op ---------
	// These are exactly the reads whatsmeow's Upgrade performs when the schema
	// is already at the latest version (catalog probes + the version SELECT).
	// They must all succeed under runtime — proving the app can boot as
	// wa_session_runtime without DDL (ADR-0108 "Boot behaviour…").
	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		                WHERE table_schema='public' AND table_name='state_groups_state')`).
		Scan(&exists); err != nil {
		t.Fatalf("runtime catalog probe (checkDatabaseOwner): %v", err)
	}
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                WHERE table_schema='public' AND table_name='whatsmeow_version'
		                  AND column_name='compat')`).
		Scan(&exists); err != nil {
		t.Fatalf("runtime catalog probe (compat column): %v", err)
	}
	var version, compat int
	if err := conn.QueryRow(ctx,
		`SELECT version, compat FROM whatsmeow_version LIMIT 1`).Scan(&version, &compat); err != nil {
		t.Fatalf("runtime SELECT whatsmeow_version (boot no-op read): %v", err)
	}
	if version != 14 || compat != 1 {
		t.Fatalf("whatsmeow_version = (%d,%d), want (14,1)", version, compat)
	}
	resetRole()

	// --- app_runtime has NO reach into the session credential store ----------
	// (Same-cluster defense in depth: the migration REVOKEs the app roles.)
	setRole("app_runtime")
	_, err = conn.Exec(ctx, `SELECT 1 FROM whatsmeow_device LIMIT 1`)
	expectNoAccess(t, err, "app_runtime SELECT whatsmeow_device")
	_, err = conn.Exec(ctx, `SELECT 1 FROM whatsmeow_preexisting LIMIT 1`)
	expectNoAccess(t, err, "app_runtime SELECT whatsmeow_preexisting")
	resetRole()
}

func TestWASessionRolesAreLeastPrivilegeShape(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db := harness.DB(t)
	su := db.SuperuserPool()
	applyWASessionRoles(t, ctx, su)

	for _, role := range []string{"wa_session_runtime", "wa_session_admin"} {
		var canLogin, super, createRole, createDB, bypassRLS bool
		err := su.QueryRow(ctx,
			`SELECT rolcanlogin, rolsuper, rolcreaterole, rolcreatedb, rolbypassrls
			   FROM pg_roles WHERE rolname = $1`, role).
			Scan(&canLogin, &super, &createRole, &createDB, &bypassRLS)
		if err != nil {
			t.Fatalf("%s: read pg_roles: %v", role, err)
		}
		if !canLogin {
			t.Errorf("%s: rolcanlogin = false, want true (it is a login role)", role)
		}
		if super {
			t.Errorf("%s: rolsuper = true, want false (NOSUPERUSER)", role)
		}
		if createRole {
			t.Errorf("%s: rolcreaterole = true, want false (NOCREATEROLE)", role)
		}
		if createDB {
			t.Errorf("%s: rolcreatedb = true, want false (NOCREATEDB)", role)
		}
		if bypassRLS {
			t.Errorf("%s: rolbypassrls = true, want false (NOBYPASSRLS)", role)
		}
	}
}

// TestWASessionRolesMigrationIsIdempotent re-applies the up migration and
// expects no error (DO-block guards + idempotent grants), matching the
// "idempotente" AC and the 0001_roles.up.sql convention.
func TestWASessionRolesMigrationIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db := harness.DB(t)
	su := db.SuperuserPool()
	applyWASessionRoles(t, ctx, su)
	applyWASessionRoles(t, ctx, su) // second run must not error
}

// applyWASessionRolesDown runs the down migration as the cluster superuser.
func applyWASessionRolesDown(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	path := filepath.Join(harness.MigrationsDir(), "wa_session", "0001_wa_session_roles.down.sql")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply wa_session roles down migration: %v", err)
	}
}

// TestWASessionRolesDownReverses proves the down migration removes both roles
// and is idempotent (re-runnable, and runnable when the roles are absent). It
// restores the up state at the end so cluster-global role state is left as the
// rest of the suite expects.
func TestWASessionRolesDownReverses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db := harness.DB(t)
	su := db.SuperuserPool()

	applyWASessionRoles(t, ctx, su)
	applyWASessionRolesDown(t, ctx, su)

	for _, role := range []string{"wa_session_runtime", "wa_session_admin"} {
		var present bool
		if err := su.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&present); err != nil {
			t.Fatalf("%s: read pg_roles: %v", role, err)
		}
		if present {
			t.Errorf("%s: still present after down migration, want dropped", role)
		}
	}

	applyWASessionRolesDown(t, ctx, su) // idempotent: down again with roles gone
	applyWASessionRoles(t, ctx, su)     // restore for the rest of the cluster lifetime
}
