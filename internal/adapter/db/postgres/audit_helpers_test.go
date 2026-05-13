package postgres_test

// SIN-62525 / [SIN-62424](/SIN/issues/SIN-62424) Phase B.2: shared
// helpers for the postgres audit integration tests. Re-landed here in
// batch 16 because audit_logger_split_test.go references them; future
// re-landing batches (app_audit_role_test.go from 0078, drop_audit_log_test.go
// from 0086) will continue to consume them. Kept in a dedicated file
// so the helpers' lifetime is independent of any one test file.

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
