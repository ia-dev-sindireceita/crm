package postgres_test

// SIN-63342 acceptance for migration 0114_users_role_check:
//
//   AC #1 — INSERT with an invalid role is rejected by the new
//           CHECK constraint regardless of the application layer.
//   AC #2 — INSERT with each allowlisted role lands cleanly.
//   AC #3 — the column DEFAULT is 'tenant_common' (least-privilege).
//   AC #4 — Step 1 backfill rewrites legacy 'agent' rows to
//           'tenant_common' so the seed is constraint-compliant.
//   AC #5 — up/down/up idempotent.
//
// Lives in postgres_test so it shares the cluster bootstrap state
// (see memory `testpg shared-cluster ALTER ROLE race`).
//
// Defense-in-depth backstop for the application-layer tenant role
// allowlist landed in SIN-63336 (see internal/iam/login.go and the
// regression in internal/iam/login_role_test.go). The CHECK ensures
// that even if a future writer bypasses the iam-layer downgrade —
// new code, ORM misuse, hand-run SQL during incident response, a
// future tenant self-service UI — the storage layer rejects the
// invalid value at INSERT/UPDATE time.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

// freshDBWithUsersRoleCheck applies the minimum migration chain that
// 0114 needs (tenants, users, then 0114 itself).
func freshDBWithUsersRoleCheck(t *testing.T) (*testpg.DB, context.Context) {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0114_users_role_check.up.sql",
	)
	return db, ctx
}

// seedRoleCheckTenant inserts a tenant via the admin pool so the
// tenant-scoped INSERTs below have a valid FK target.
func seedRoleCheckTenant(t *testing.T, ctx context.Context, db *testpg.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, "tnt-"+id.String()[:8], "h-"+id.String()[:8]+".test"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// AC #1 — invalid role is rejected by the CHECK constraint.
// ---------------------------------------------------------------------------

// TestUsersRoleCHECK_RejectsInvalidValue encodes the vulnerability:
// an attempt to write role='master_pretender' (or any garbage value
// not in the allowlist) MUST be rejected by the DB layer, regardless
// of what the application does. The application-layer downgrade from
// SIN-63336 remains the primary defense; this is the schema-layer
// backstop.
func TestUsersRoleCHECK_RejectsInvalidValue(t *testing.T) {
	db, ctx := freshDBWithUsersRoleCheck(t)
	tenantID := seedRoleCheckTenant(t, ctx, db)

	cases := []string{
		"master_pretender",
		"agent", // pre-0114 legacy value
		"",      // empty string
		"ADMIN", // wrong case — allowlist is case-sensitive
		"garbage",
	}

	for _, value := range cases {
		t.Run("rejects_"+value, func(t *testing.T) {
			userID := uuid.New()
			_, err := db.AdminPool().Exec(ctx,
				`INSERT INTO users (id, tenant_id, email, password_hash, role)
				 VALUES ($1, $2, $3, 'x', $4)`,
				userID, tenantID, userID.String()+"@check.test", value)
			if err == nil {
				t.Fatalf("INSERT with role=%q unexpectedly succeeded", value)
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("INSERT with role=%q: error is not a *pgconn.PgError: %v", value, err)
			}
			// SQLSTATE 23514 is "check_violation".
			if pgErr.Code != "23514" {
				t.Fatalf("INSERT with role=%q: SQLSTATE=%s want 23514; msg=%s",
					value, pgErr.Code, pgErr.Message)
			}
			if !strings.Contains(pgErr.ConstraintName, "users_role_chk") {
				t.Fatalf("INSERT with role=%q: constraint=%q want users_role_chk; msg=%s",
					value, pgErr.ConstraintName, pgErr.Message)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC #2 — every allowlisted role inserts cleanly.
// ---------------------------------------------------------------------------

// TestUsersRoleCHECK_AcceptsAllAllowlistedValues sweeps the canonical
// values the constraint allowlists. Reflects the as-built allowlist
// in 0114_users_role_check.up.sql, mirrored from sessions_role_check
// in 0077_session_activity.up.sql plus 'admin' for the MFA-requirement
// marker read by user_mfa_requirement.go (AdminRole).
func TestUsersRoleCHECK_AcceptsAllAllowlistedValues(t *testing.T) {
	db, ctx := freshDBWithUsersRoleCheck(t)
	tenantID := seedRoleCheckTenant(t, ctx, db)

	tenantRoles := []string{"tenant_gerente", "tenant_atendente", "tenant_common", "admin"}
	for _, role := range tenantRoles {
		t.Run("accepts_"+role, func(t *testing.T) {
			userID := uuid.New()
			if _, err := db.AdminPool().Exec(ctx,
				`INSERT INTO users (id, tenant_id, email, password_hash, role)
				 VALUES ($1, $2, $3, 'x', $4)`,
				userID, tenantID, userID.String()+"@allow.test", role); err != nil {
				t.Fatalf("INSERT role=%q rejected unexpectedly: %v", role, err)
			}
		})
	}

	// 'master' is allowlisted but must land on a master row
	// (is_master=true, tenant_id IS NULL) to satisfy the pre-existing
	// users_master_xor_tenant CHECK from 0005. Asserting it here
	// proves the new CHECK does not regress the master path.
	t.Run("accepts_master_on_master_row", func(t *testing.T) {
		userID := uuid.New()
		if _, err := db.AdminPool().Exec(ctx,
			`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
			 VALUES ($1, NULL, $2, 'x', 'master', true)`,
			userID, userID.String()+"@master.test"); err != nil {
			t.Fatalf("INSERT role=master on master row rejected: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// AC #3 — column DEFAULT is 'tenant_common' after the migration.
// ---------------------------------------------------------------------------

// TestUsersRoleCHECK_DefaultIsTenantCommon asserts that a naive
// INSERT without an explicit role lands on a value that survives
// the CHECK. Prior to 0114 the DEFAULT was 'agent', which the
// constraint now rejects — leaving the old default would brick
// every naive INSERT.
func TestUsersRoleCHECK_DefaultIsTenantCommon(t *testing.T) {
	db, ctx := freshDBWithUsersRoleCheck(t)
	tenantID := seedRoleCheckTenant(t, ctx, db)

	userID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash)
		 VALUES ($1, $2, $3, 'x')`,
		userID, tenantID, userID.String()+"@default.test"); err != nil {
		t.Fatalf("INSERT without explicit role: %v", err)
	}

	var role string
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT role FROM users WHERE id = $1`, userID).Scan(&role); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if role != "tenant_common" {
		t.Fatalf("DEFAULT role = %q, want tenant_common", role)
	}
}

// ---------------------------------------------------------------------------
// AC #4 — Step 1 backfill rewrites legacy 'agent' rows.
// ---------------------------------------------------------------------------

// TestUsersRoleCHECK_BackfillsLegacyAgentRows seeds pre-migration
// state (users.role='agent' on a tenant row) using 0005 only, then
// applies 0114 and asserts the row was rewritten to 'tenant_common'.
// This is the path that lets the constraint apply at all — without
// the backfill, the staging snapshot would fail Step 3 hard.
func TestUsersRoleCHECK_BackfillsLegacyAgentRows(t *testing.T) {
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
	)

	tenantID := seedRoleCheckTenant(t, ctx, db)
	legacyUserID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role)
		 VALUES ($1, $2, $3, 'x', 'agent')`,
		legacyUserID, tenantID, legacyUserID.String()+"@legacy.test"); err != nil {
		t.Fatalf("seed legacy 'agent' row: %v", err)
	}
	// Master row carrying role='master' must survive the backfill
	// untouched — only is_master=false rows are rewritten.
	masterID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, NULL, $2, 'x', 'master', true)`,
		masterID, masterID.String()+"@master-keep.test"); err != nil {
		t.Fatalf("seed master row: %v", err)
	}

	applyChain(t, ctx, db, "0114_users_role_check.up.sql")

	var got string
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT role FROM users WHERE id = $1`, legacyUserID).Scan(&got); err != nil {
		t.Fatalf("read legacy row after up: %v", err)
	}
	if got != "tenant_common" {
		t.Fatalf("legacy 'agent' row = %q after backfill, want tenant_common", got)
	}
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT role FROM users WHERE id = $1`, masterID).Scan(&got); err != nil {
		t.Fatalf("read master row after up: %v", err)
	}
	if got != "master" {
		t.Fatalf("master row = %q after backfill, want master (unchanged)", got)
	}
}

// ---------------------------------------------------------------------------
// AC #5 — up/down/up idempotency.
// ---------------------------------------------------------------------------

// TestUsersRoleCHECKMigration_UpDownUp re-applies the migration in
// both directions to prove DROP CONSTRAINT IF EXISTS guards the
// rerun path. The backfill UPDATE is included in down so re-applying
// up reproduces the same end state.
func TestUsersRoleCHECKMigration_UpDownUp(t *testing.T) {
	db, ctx := freshDBWithUsersRoleCheck(t)

	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(),
		"0114_users_role_check.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(),
		"0114_users_role_check.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}

	if !usersRoleConstraintPresent(t, ctx, db) {
		t.Fatalf("after initial up: users_role_chk missing")
	}

	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if usersRoleConstraintPresent(t, ctx, db) {
		t.Fatalf("after down: users_role_chk still present")
	}

	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if !usersRoleConstraintPresent(t, ctx, db) {
		t.Fatalf("after re-up: users_role_chk missing")
	}

	// down + down (idempotent) then up + up (idempotent).
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

func usersRoleConstraintPresent(t *testing.T, ctx context.Context, db *testpg.DB) bool {
	t.Helper()
	var n int
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint
		  WHERE conname = 'users_role_chk' AND conrelid = 'public.users'::regclass`).
		Scan(&n); err != nil {
		t.Fatalf("constraint probe: %v", err)
	}
	return n == 1
}
