package postgres_test

// SIN-66499 integration tests for the tenantusers Postgres adapter +
// migration 0130 (users.active, users.must_change_password) and the login
// active-check added to UserCredentialReader.
//
// These live in the parent postgres_test package (not the
// internal/adapter/db/postgres/tenantusers subpackage) so they share the
// TestMain / harness with the other adapter tests and dodge the
// shared-cluster ALTER ROLE race a second test binary would trigger
// (reference_testpg_shared_cluster_race; same rationale as
// channels_adapter_test.go).

import (
	"context"
	"errors"
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

// tenantUsersMigrationChain is the migration set the surface needs: tenants
// (0004), users (0005), the role CHECK (0114), then this feature (0130).
var tenantUsersMigrationChain = []string{
	"0004_create_tenant.up.sql",
	"0005_create_users.up.sql",
	"0114_users_role_check.up.sql",
	"0130_users_active_must_change_password.up.sql",
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

func seedTenantForUsers(t *testing.T, db *testpg.DB) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, fmt.Sprintf("t-%s", id), fmt.Sprintf("%s.crm.local", id)); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func newTenantUsersStore(t *testing.T, db *testpg.DB) *pgtenantusers.Store {
	t.Helper()
	s, err := pgtenantusers.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pgtenantusers.New: %v", err)
	}
	return s
}

func mkTenantUser(t *testing.T, tenant uuid.UUID, email string, role iam.Role) *tenantusers.User {
	t.Helper()
	u, err := tenantusers.New(tenant, email, role, "argon2id$fake$hash")
	if err != nil {
		t.Fatalf("tenantusers.New: %v", err)
	}
	return u
}

func TestTenantUsersAdapter_New_RejectsNilPool(t *testing.T) {
	if _, err := pgtenantusers.New(nil); err == nil {
		t.Error("New(nil) err = nil, want postgres.ErrNilPool")
	}
}

func TestTenantUsersAdapter_CreateListGet(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	ctx := context.Background()
	tenant := seedTenantForUsers(t, db)
	store := newTenantUsersStore(t, db)

	g := mkTenantUser(t, tenant, "gerente@acme.com", iam.RoleTenantGerente)
	a := mkTenantUser(t, tenant, "atendente@acme.com", iam.RoleTenantAtendente)
	if err := store.Create(ctx, g); err != nil {
		t.Fatalf("Create gerente: %v", err)
	}
	if err := store.Create(ctx, a); err != nil {
		t.Fatalf("Create atendente: %v", err)
	}

	list, err := store.List(ctx, tenant)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List len = %d, want 2", len(list))
	}
	// Ordered by email: atendente@ before gerente@.
	if list[0].Email != "atendente@acme.com" || list[1].Email != "gerente@acme.com" {
		t.Fatalf("List order = [%s %s], want atendente then gerente", list[0].Email, list[1].Email)
	}
	// List must not carry the password hash back into the domain.
	if list[0].PasswordHash != "" {
		t.Errorf("List leaked password hash")
	}

	got, err := store.Get(ctx, tenant, g.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Email != "gerente@acme.com" || got.Role != iam.RoleTenantGerente || !got.Active || !got.MustChangePassword {
		t.Fatalf("Get = %+v, want active gerente pending password change", got)
	}

	// is_master must be false for every user this adapter creates.
	var isMaster bool
	if err := db.AdminPool().QueryRow(ctx, `SELECT is_master FROM users WHERE id = $1`, g.ID).Scan(&isMaster); err != nil {
		t.Fatalf("read is_master: %v", err)
	}
	if isMaster {
		t.Fatal("adapter set is_master=true — anti-escalation violated")
	}
}

func TestTenantUsersAdapter_EmailConflict(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	ctx := context.Background()
	tenant := seedTenantForUsers(t, db)
	store := newTenantUsersStore(t, db)

	if err := store.Create(ctx, mkTenantUser(t, tenant, "dup@acme.com", iam.RoleTenantAtendente)); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := store.Create(ctx, mkTenantUser(t, tenant, "dup@acme.com", iam.RoleTenantAtendente))
	if !errors.Is(err, tenantusers.ErrEmailConflict) {
		t.Fatalf("second create err = %v, want ErrEmailConflict", err)
	}
	// Same email in a DIFFERENT tenant is allowed (unique is per-tenant).
	other := seedTenantForUsers(t, db)
	if err := store.Create(ctx, mkTenantUser(t, other, "dup@acme.com", iam.RoleTenantAtendente)); err != nil {
		t.Fatalf("cross-tenant same-email create err = %v, want nil", err)
	}
}

func TestTenantUsersAdapter_CrossTenantIsolation(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	ctx := context.Background()
	a := seedTenantForUsers(t, db)
	b := seedTenantForUsers(t, db)
	store := newTenantUsersStore(t, db)

	ua := mkTenantUser(t, a, "ua@acme.com", iam.RoleTenantGerente)
	ub := mkTenantUser(t, b, "ub@acme.com", iam.RoleTenantGerente)
	if err := store.Create(ctx, ua); err != nil {
		t.Fatalf("create ua: %v", err)
	}
	if err := store.Create(ctx, ub); err != nil {
		t.Fatalf("create ub: %v", err)
	}

	// Tenant A cannot see tenant B's user.
	if _, err := store.Get(ctx, a, ub.ID); !errors.Is(err, tenantusers.ErrNotFound) {
		t.Fatalf("Get(A, B-user) err = %v, want ErrNotFound", err)
	}
	listA, _ := store.List(ctx, a)
	if len(listA) != 1 || listA[0].ID != ua.ID {
		t.Fatalf("List(A) leaked cross-tenant rows: %+v", listA)
	}
	// Tenant A cannot mutate tenant B's user.
	if err := store.UpdateRole(ctx, a, ub.ID, iam.RoleTenantAtendente); !errors.Is(err, tenantusers.ErrNotFound) {
		t.Fatalf("UpdateRole(A, B-user) err = %v, want ErrNotFound", err)
	}
	if err := store.SetActive(ctx, a, ub.ID, false); !errors.Is(err, tenantusers.ErrNotFound) {
		t.Fatalf("SetActive(A, B-user) err = %v, want ErrNotFound", err)
	}
}

func TestTenantUsersAdapter_UpdateRoleAndSetActive(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	ctx := context.Background()
	tenant := seedTenantForUsers(t, db)
	store := newTenantUsersStore(t, db)

	u := mkTenantUser(t, tenant, "u@acme.com", iam.RoleTenantAtendente)
	if err := store.Create(ctx, u); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := store.UpdateRole(ctx, tenant, u.ID, iam.RoleTenantGerente); err != nil {
		t.Fatalf("UpdateRole: %v", err)
	}
	got, _ := store.Get(ctx, tenant, u.ID)
	if got.Role != iam.RoleTenantGerente {
		t.Fatalf("role = %q, want gerente", got.Role)
	}

	if err := store.SetActive(ctx, tenant, u.ID, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	got, _ = store.Get(ctx, tenant, u.ID)
	if got.Active {
		t.Fatal("user should be inactive after SetActive(false)")
	}

	// Missing rows report ErrNotFound.
	if err := store.UpdateRole(ctx, tenant, uuid.New(), iam.RoleTenantGerente); !errors.Is(err, tenantusers.ErrNotFound) {
		t.Fatalf("UpdateRole(missing) err = %v, want ErrNotFound", err)
	}
	if err := store.SetActive(ctx, tenant, uuid.New(), true); !errors.Is(err, tenantusers.ErrNotFound) {
		t.Fatalf("SetActive(missing) err = %v, want ErrNotFound", err)
	}
}

func TestTenantUsersAdapter_CountActiveGerentes(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	ctx := context.Background()
	tenant := seedTenantForUsers(t, db)
	store := newTenantUsersStore(t, db)

	g1 := mkTenantUser(t, tenant, "g1@acme.com", iam.RoleTenantGerente)
	g2 := mkTenantUser(t, tenant, "g2@acme.com", iam.RoleTenantGerente)
	a1 := mkTenantUser(t, tenant, "a1@acme.com", iam.RoleTenantAtendente)
	for _, u := range []*tenantusers.User{g1, g2, a1} {
		if err := store.Create(ctx, u); err != nil {
			t.Fatalf("create %s: %v", u.Email, err)
		}
	}
	n, err := store.CountActiveGerentes(ctx, tenant)
	if err != nil {
		t.Fatalf("CountActiveGerentes: %v", err)
	}
	if n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
	// Deactivating one gerente drops the active count.
	if err := store.SetActive(ctx, tenant, g1.ID, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	n, _ = store.CountActiveGerentes(ctx, tenant)
	if n != 1 {
		t.Fatalf("count after deactivate = %d, want 1", n)
	}
}

// TestTenantUsersAdapter_LoginActiveCheck locks the SIN-66499 login gate: a
// deactivated user (active=false) is invisible to the login credential
// lookup (returns the uuid.Nil "not found" sentinel), while an active user
// resolves normally. This proves the `AND active` predicate added to
// UserCredentialReader.LookupCredentials.
func TestTenantUsersAdapter_LoginActiveCheck(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	ctx := context.Background()
	tenant := seedTenantForUsers(t, db)
	store := newTenantUsersStore(t, db)
	cred := postgres.NewUserCredentialReader(db.RuntimePool())

	u := mkTenantUser(t, tenant, "loginme@acme.com", iam.RoleTenantAtendente)
	if err := store.Create(ctx, u); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Active user resolves.
	id, hash, err := cred.LookupCredentials(ctx, tenant, "loginme@acme.com")
	if err != nil {
		t.Fatalf("LookupCredentials(active): %v", err)
	}
	if id != u.ID || hash == "" {
		t.Fatalf("active lookup = (%v, %q), want (%v, non-empty)", id, hash, u.ID)
	}

	// Deactivate → the same lookup now returns the not-found sentinel.
	if err := store.SetActive(ctx, tenant, u.ID, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	id, _, err = cred.LookupCredentials(ctx, tenant, "loginme@acme.com")
	if err != nil {
		t.Fatalf("LookupCredentials(inactive): %v", err)
	}
	if id != uuid.Nil {
		t.Fatalf("deactivated lookup id = %v, want uuid.Nil (not found)", id)
	}
}
