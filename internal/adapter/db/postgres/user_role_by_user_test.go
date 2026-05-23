package postgres_test

// SIN-63336 postgres-side coverage for UserCredentialReader.RoleByUser.
//
// The adapter widens the existing user-store port so iam.Service.Login can
// stamp the persisted users.role on the freshly created session. Three
// cases pin the contract:
//
//   - Hit: a user with role='tenant_gerente' round-trips as
//     iam.RoleTenantGerente. The raw string is returned verbatim so the
//     iam layer can apply the SIN-63340 §Item 1 tenant allowlist (which
//     deliberately excludes 'master' to block STRIDE-E).
//   - Miss: querying a userID that does not exist returns the zero Role
//     ("") with a nil error. iam.Login then defaults to RoleTenantCommon.
//   - Cross-tenant: a user that exists under tenantA is invisible from
//     tenantB via the app_runtime RLS-bound pool. The SELECT collapses to
//     the same zero-Role no-error path so an attacker cannot enumerate
//     role assignments across tenants.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/iam"
)

func TestUserCredentialReader_RoleByUser_Hit(t *testing.T) {
	db := freshDBWithIAM(t)
	ctx := context.Background()
	tenantID := uuid.New()
	userID := uuid.New()
	hash, err := iam.HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantID, "acme.crm.local", "acme.crm.local"); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, $4, 'tenant_gerente')`,
		userID, tenantID, "admin@acme.test", hash); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	reader := postgres.NewUserCredentialReader(db.RuntimePool())
	got, err := reader.RoleByUser(ctx, tenantID, userID)
	if err != nil {
		t.Fatalf("RoleByUser: %v", err)
	}
	if got != iam.RoleTenantGerente {
		t.Fatalf("Role=%q, want %q", got, iam.RoleTenantGerente)
	}
}

func TestUserCredentialReader_RoleByUser_Miss_ReturnsZeroNoError(t *testing.T) {
	db := freshDBWithIAM(t)
	tenantID, _, _ := seedTenant(t, db, "acme.crm.local", "alice@acme.test")
	reader := postgres.NewUserCredentialReader(db.RuntimePool())
	// Unknown userID under a known tenant — the SELECT returns no rows
	// and the contract is (Role(""), nil), not a typed error.
	got, err := reader.RoleByUser(context.Background(), tenantID, uuid.New())
	if err != nil {
		t.Fatalf("contract violation: miss must return nil error, got %v", err)
	}
	if got != iam.Role("") {
		t.Fatalf("miss must return zero Role, got %q", got)
	}
}

// TestUserCredentialReader_RoleByUser_CrossTenant_HiddenByRLS proves a
// user that exists under tenantA cannot be probed from tenantB via the
// runtime pool — the SELECT on users.role is filtered by the same RLS
// policy that gates LookupCredentials.
func TestUserCredentialReader_RoleByUser_CrossTenant_HiddenByRLS(t *testing.T) {
	db := freshDBWithIAM(t)
	ctx := context.Background()
	tenantA := uuid.New()
	userA := uuid.New()
	hash, err := iam.HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantA, "acme.crm.local", "acme.crm.local"); err != nil {
		t.Fatalf("insert tenant A: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, $4, 'tenant_gerente')`,
		userA, tenantA, "admin@acme.test", hash); err != nil {
		t.Fatalf("insert user A: %v", err)
	}
	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "globex.crm.local", "globex.crm.local"); err != nil {
		t.Fatalf("insert tenant B: %v", err)
	}

	reader := postgres.NewUserCredentialReader(db.RuntimePool())
	got, err := reader.RoleByUser(ctx, tenantB, userA)
	if err != nil {
		t.Fatalf("RoleByUser: %v", err)
	}
	if got != iam.Role("") {
		t.Fatalf("cross-tenant probe leaked: role=%q (must be empty under RLS)", got)
	}
}
