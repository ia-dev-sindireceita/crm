package postgres_test

// SIN-66496 integration tests for the tenantusers Postgres adapter. They run
// against the shared testpg cluster in the parent postgres_test package (same
// shared-cluster rationale as channels_adapter_test.go — a NEW test binary
// would race ALTER ROLE on the CI cluster). Coverage of the tenantusers
// adapter package is measured by CI's -coverpkg sweep, mirroring how the
// channels / consent adapters are tested from this package.
//
// Focus:
//   - tenant isolation: a Store scoped to tenant A cannot see / touch tenant
//     B's rows (RLS + code).
//   - create both assignable roles + invite token row; duplicate email is
//     rejected (ErrEmailTaken).
//   - anti-lockout (G4): the last active gerente cannot be demoted or
//     deactivated; adding a second gerente unblocks it.
//   - soft-deactivation blocks login (LookupCredentials filters it out).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	postgres "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgtenantusers "github.com/pericles-luz/crm/internal/adapter/db/postgres/tenantusers"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenantusers"
)

var tenantUsersMigrationChain = []string{
	"0004_create_tenant.up.sql",
	"0005_create_users.up.sql",
	"0134_users_deactivated_at.up.sql",
	"0135_user_credential_tokens.up.sql",
}

func freshDBWithTenantUsers(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range tenantUsersMigrationChain {
		applyMigration(t, db, ctx, name)
	}
	return db
}

func seedTUTenant(t *testing.T, db *testpg.DB) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, fmt.Sprintf("tu-%s", id), fmt.Sprintf("tu-%s.crm.local", id)); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

// seedTUUser inserts a user directly (admin pool, bypassing RLS) so tests can
// arrange fixtures without going through the invite flow.
func seedTUUser(t *testing.T, db *testpg.DB, tenantID uuid.UUID, role iam.Role, email string) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, $2, $3, 'x', $4, false)`,
		id, tenantID, email, string(role)); err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
	return id
}

func newTUStore(t *testing.T, db *testpg.DB) *pgtenantusers.Store {
	t.Helper()
	s, err := pgtenantusers.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pgtenantusers.New: %v", err)
	}
	return s
}

func mkToken() tenantusers.Token {
	return tenantusers.Token{
		Plaintext: "plain",
		SHA256:    tenantusers.HashToken(uuid.NewString()),
		Purpose:   tenantusers.PurposeInvite,
		ExpiresAt: time.Now().Add(tenantusers.InviteTTL),
	}
}

func TestTenantUsersAdapter_New_RejectsNilPool(t *testing.T) {
	if _, err := pgtenantusers.New(nil); err == nil {
		t.Error("New(nil) err = nil, want ErrNilPool")
	}
}

func TestTenantUsersAdapter_CreateBothRolesAndToken(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	store := newTUStore(t, db)
	tenant := seedTUTenant(t, db)
	ctx := context.Background()

	for _, role := range []iam.Role{iam.RoleTenantAtendente, iam.RoleTenantGerente} {
		u := tenantusers.User{ID: uuid.New(), TenantID: tenant, Email: fmt.Sprintf("%s@acme.example", role), Role: role, CreatedAt: time.Now()}
		if err := store.Create(ctx, u, "argon2id$placeholder", mkToken()); err != nil {
			t.Fatalf("Create(%s): %v", role, err)
		}
		got, err := store.Get(ctx, tenant, u.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", role, err)
		}
		if got.Role != role || !got.Active() {
			t.Fatalf("stored user = %+v, want role %s active", got, role)
		}
	}

	// Token rows exist for the created users.
	var n int
	if err := db.AdminPool().QueryRow(ctx, `SELECT count(*) FROM user_credential_tokens WHERE tenant_id = $1`, tenant).Scan(&n); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if n != 2 {
		t.Fatalf("token rows = %d, want 2", n)
	}
}

func TestTenantUsersAdapter_DuplicateEmail(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	store := newTUStore(t, db)
	tenant := seedTUTenant(t, db)
	ctx := context.Background()

	u := tenantusers.User{ID: uuid.New(), TenantID: tenant, Email: "dup@acme.example", Role: iam.RoleTenantAtendente, CreatedAt: time.Now()}
	if err := store.Create(ctx, u, "x", mkToken()); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	u2 := tenantusers.User{ID: uuid.New(), TenantID: tenant, Email: "dup@acme.example", Role: iam.RoleTenantAtendente, CreatedAt: time.Now()}
	if err := store.Create(ctx, u2, "x", mkToken()); err != tenantusers.ErrEmailTaken {
		t.Fatalf("duplicate Create err = %v, want ErrEmailTaken", err)
	}
}

func TestTenantUsersAdapter_TenantIsolation(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	tenantA := seedTUTenant(t, db)
	tenantB := seedTUTenant(t, db)
	// Store scoped to tenant A.
	storeA := newTUStore(t, db)
	ctx := context.Background()

	seedTUUser(t, db, tenantA, iam.RoleTenantGerente, "a-boss@x")
	bID := seedTUUser(t, db, tenantB, iam.RoleTenantAtendente, "b-agent@x")

	// List(A) must not include tenant B rows.
	list, err := storeA.List(ctx, tenantA)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, u := range list {
		if u.TenantID != tenantA {
			t.Fatalf("List leaked cross-tenant row: %+v", u)
		}
	}

	// Get / UpdateRole / Deactivate of a tenant-B id under tenant-A scope must
	// resolve to not-found (RLS + code), never touch B's row.
	if _, err := storeA.Get(ctx, tenantA, bID); err != tenantusers.ErrUserNotFound {
		t.Fatalf("cross-tenant Get err = %v, want ErrUserNotFound", err)
	}
	if _, err := storeA.UpdateRole(ctx, tenantA, bID, iam.RoleTenantGerente); err != tenantusers.ErrUserNotFound {
		t.Fatalf("cross-tenant UpdateRole err = %v, want ErrUserNotFound", err)
	}
	if _, err := storeA.Deactivate(ctx, tenantA, bID); err != tenantusers.ErrUserNotFound {
		t.Fatalf("cross-tenant Deactivate err = %v, want ErrUserNotFound", err)
	}
}

func TestTenantUsersAdapter_AntiLockout(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	store := newTUStore(t, db)
	tenant := seedTUTenant(t, db)
	ctx := context.Background()

	g1 := seedTUUser(t, db, tenant, iam.RoleTenantGerente, "g1@x")

	// Only gerente: cannot demote nor deactivate.
	if _, err := store.UpdateRole(ctx, tenant, g1, iam.RoleTenantAtendente); err != tenantusers.ErrLastGerente {
		t.Fatalf("demote last gerente err = %v, want ErrLastGerente", err)
	}
	if _, err := store.Deactivate(ctx, tenant, g1); err != tenantusers.ErrLastGerente {
		t.Fatalf("deactivate last gerente err = %v, want ErrLastGerente", err)
	}

	// Add a second gerente → now g1 can be deactivated.
	seedTUUser(t, db, tenant, iam.RoleTenantGerente, "g2@x")
	if _, err := store.Deactivate(ctx, tenant, g1); err != nil {
		t.Fatalf("deactivate with 2 gerentes: %v", err)
	}
	// Reactivate restores it.
	if _, err := store.Reactivate(ctx, tenant, g1); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	got, _ := store.Get(ctx, tenant, g1)
	if !got.Active() {
		t.Fatal("user should be active after reactivate")
	}
}

func TestTenantUsersAdapter_DeactivateBlocksLogin(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	store := newTUStore(t, db)
	reader := postgres.NewUserCredentialReader(db.RuntimePool())
	tenant := seedTUTenant(t, db)
	ctx := context.Background()

	// A second gerente so the target can be deactivated.
	uID := seedTUUser(t, db, tenant, iam.RoleTenantGerente, "login@x")
	seedTUUser(t, db, tenant, iam.RoleTenantGerente, "other@x")

	// Before: login lookup finds the user.
	id, _, err := reader.LookupCredentials(ctx, tenant, "login@x")
	if err != nil || id != uID {
		t.Fatalf("pre-deactivate lookup = (%s, %v), want %s", id, err, uID)
	}

	if _, err := store.Deactivate(ctx, tenant, uID); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	// After: the deactivated user is un-findable → login denied (no enumeration).
	id2, _, err := reader.LookupCredentials(ctx, tenant, "login@x")
	if err != nil {
		t.Fatalf("post-deactivate lookup err = %v", err)
	}
	if id2 != uuid.Nil {
		t.Fatalf("deactivated user still found: %s", id2)
	}
}
