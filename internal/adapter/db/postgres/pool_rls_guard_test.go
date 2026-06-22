package postgres_test

import (
	"errors"
	"testing"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
)

// TestEnforceRuntimeRLSRoleFromEnv_RealRoles drives the SIN-65590 boot guard
// against the REAL pg_roles catalog for each application role (rule 5: no
// mocking the DB for code that touches storage). It proves the guard reads
// rolsuper/rolbypassrls correctly per role and applies the enforcement
// policy:
//
//   - app_runtime (NOBYPASSRLS)            → boots clean even with the flag on.
//   - app_admin   (BYPASSRLS, the migrator) → WARNs always; hard-fails only
//     when DB_ENFORCE_RLS_ROLE=1.
func TestEnforceRuntimeRLSRoleFromEnv_RealRoles(t *testing.T) {
	t.Parallel()
	db := harness.DB(t)
	ctx := newCtx(t)

	env := func(dsn, enforce string) func(string) string {
		return func(name string) string {
			switch name {
			case postgresadapter.EnvDSN:
				return dsn
			case postgresadapter.EnvEnforceRLSRole:
				return enforce
			default:
				return ""
			}
		}
	}

	t.Run("app_runtime passes with enforcement on", func(t *testing.T) {
		if err := postgresadapter.EnforceRuntimeRLSRoleFromEnv(ctx, env(db.RuntimeDSN(), "1")); err != nil {
			t.Fatalf("app_runtime under enforcement: got %v, want nil", err)
		}
	})

	t.Run("app_admin hard-fails with enforcement on", func(t *testing.T) {
		err := postgresadapter.EnforceRuntimeRLSRoleFromEnv(ctx, env(db.AdminDSN(), "1"))
		if !errors.Is(err, postgresadapter.ErrRuntimeRoleBypassesRLS) {
			t.Fatalf("app_admin under enforcement: got %v, want ErrRuntimeRoleBypassesRLS", err)
		}
	})

	t.Run("app_admin only warns with enforcement off", func(t *testing.T) {
		if err := postgresadapter.EnforceRuntimeRLSRoleFromEnv(ctx, env(db.AdminDSN(), "")); err != nil {
			t.Fatalf("app_admin without enforcement: got %v, want nil (warn-only)", err)
		}
	})
}
