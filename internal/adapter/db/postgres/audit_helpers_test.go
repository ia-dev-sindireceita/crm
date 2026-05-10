package postgres_test

// SIN-62424 (Phase B.2): shared helpers for the postgres audit
// integration tests. These outlived the deletion of
// audit_logger_test.go (the legacy AuditLogger writer) because they
// are still consumed by:
//
//   - audit_logger_split_test.go (SplitAuditLogger inserts, master-event
//     null-tenant case, app_audit grants on the split tables, the
//     unified-view assertion, the up/down/up cycle for migrations
//     0012/0013).
//   - app_audit_role_test.go     (the up/down/up cycle for the
//     dedicated app_audit role created in 0009).
//   - drop_audit_log_test.go     (the up/down/up cycle for migration
//     0015 that retires the legacy audit_log table).
//
// Keeping the helpers in a dedicated file makes their lifetime
// independent of any one test file's lifecycle.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

// seedTenantUserMaster inserts a tenant and a master user. Returns
// the tenant id and the master user id. The master user is required
// because audit_log_security.actor_user_id has a FK to users(id), and
// some legacy migration tests still create rows under master context.
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

// contains reports whether s contains sub. The split tests use this on
// jsonb-rendered byte slices where standard library strings.Contains
// would also work; the helper is kept as a one-liner so the call sites
// stay readable.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// roleExists reports whether a Postgres role with the given name is
// present on the cluster. Used by the migration up/down/up cycle tests
// to assert role lifecycle.
func roleExists(t *testing.T, ctx context.Context, db *testpg.DB, name string) bool {
	t.Helper()
	var got bool
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, name).Scan(&got); err != nil {
		t.Fatalf("role probe: %v", err)
	}
	return got
}
