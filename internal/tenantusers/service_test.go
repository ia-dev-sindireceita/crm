package tenantusers_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenantusers"
)

// fakeRepo is an in-memory Repository. It matches production semantics
// (tenant-scoped reads/writes, ErrEmailConflict on duplicate email,
// ErrNotFound on missing rows) so the Service is exercised against a
// documented in-memory adapter — not a mocked database. The real Postgres
// adapter has its own CI-only integration tests.
type fakeRepo struct {
	mu    sync.Mutex
	users map[uuid.UUID]*tenantusers.User // keyed by user id
}

func newFakeRepo() *fakeRepo { return &fakeRepo{users: map[uuid.UUID]*tenantusers.User{}} }

func (r *fakeRepo) List(_ context.Context, tenantID uuid.UUID) ([]*tenantusers.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*tenantusers.User
	for _, u := range r.users {
		if u.TenantID == tenantID {
			cp := *u
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *fakeRepo) Get(_ context.Context, tenantID, id uuid.UUID) (*tenantusers.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[id]
	if !ok || u.TenantID != tenantID {
		return nil, tenantusers.ErrNotFound
	}
	cp := *u
	return &cp, nil
}

func (r *fakeRepo) Create(_ context.Context, u *tenantusers.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ex := range r.users {
		if ex.TenantID == u.TenantID && ex.Email == u.Email {
			return tenantusers.ErrEmailConflict
		}
	}
	cp := *u
	r.users[u.ID] = &cp
	return nil
}

func (r *fakeRepo) UpdateRole(_ context.Context, tenantID, id uuid.UUID, role iam.Role) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[id]
	if !ok || u.TenantID != tenantID {
		return tenantusers.ErrNotFound
	}
	u.Role = role
	return nil
}

func (r *fakeRepo) SetActive(_ context.Context, tenantID, id uuid.UUID, active bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[id]
	if !ok || u.TenantID != tenantID {
		return tenantusers.ErrNotFound
	}
	u.Active = active
	return nil
}

func (r *fakeRepo) CountActiveGerentes(_ context.Context, tenantID uuid.UUID) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, u := range r.users {
		if u.TenantID == tenantID && u.Active && u.Role == iam.RoleTenantGerente {
			n++
		}
	}
	return n, nil
}

// seed inserts a user directly (bypassing Create's email check) for setup.
func (r *fakeRepo) seed(u *tenantusers.User) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *u
	r.users[u.ID] = &cp
}

type fakeHasher struct{ calls int }

func (h *fakeHasher) Hash(plain string) (string, error) {
	h.calls++
	return "argon2id$" + plain, nil
}

type auditEvent struct {
	kind           string
	actor, tenant  uuid.UUID
	target         uuid.UUID
	email          string
	from, to, role iam.Role
}

type fakeAuditor struct {
	mu     sync.Mutex
	events []auditEvent
}

func (a *fakeAuditor) UserCreated(_ context.Context, actor, tenant, target uuid.UUID, email string, role iam.Role) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, auditEvent{kind: "created", actor: actor, tenant: tenant, target: target, email: email, role: role})
}

func (a *fakeAuditor) UserRoleChanged(_ context.Context, actor, tenant, target uuid.UUID, from, to iam.Role) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, auditEvent{kind: "role_changed", actor: actor, tenant: tenant, target: target, from: from, to: to})
}

func (a *fakeAuditor) UserDeactivated(_ context.Context, actor, tenant, target uuid.UUID, role iam.Role) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, auditEvent{kind: "deactivated", actor: actor, tenant: tenant, target: target, role: role})
}

func mustService(t *testing.T, repo tenantusers.Repository, audit tenantusers.Auditor) *tenantusers.Service {
	t.Helper()
	svc, err := tenantusers.NewService(repo, &fakeHasher{}, audit)
	if err != nil {
		t.Fatalf("NewService err = %v", err)
	}
	return svc
}

func TestNewService_RequiredPorts(t *testing.T) {
	t.Parallel()
	if _, err := tenantusers.NewService(nil, &fakeHasher{}, nil); err == nil {
		t.Error("expected error for nil repo")
	}
	if _, err := tenantusers.NewService(newFakeRepo(), nil, nil); err == nil {
		t.Error("expected error for nil hasher")
	}
}

func TestCreateUser_BothRoles(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	actor := uuid.New()
	for _, role := range []iam.Role{iam.RoleTenantGerente, iam.RoleTenantAtendente} {
		repo := newFakeRepo()
		audit := &fakeAuditor{}
		svc := mustService(t, repo, audit)
		res, err := svc.CreateUser(context.Background(), tenant, actor, "New@Acme.com", role)
		if err != nil {
			t.Fatalf("CreateUser(%s) err = %v", role, err)
		}
		if res.User.Role != role {
			t.Errorf("role = %q, want %q", res.User.Role, role)
		}
		if res.User.Email != "new@acme.com" {
			t.Errorf("email = %q, want normalised", res.User.Email)
		}
		if res.TempPassword == "" {
			t.Error("expected a one-time temp password")
		}
		if !res.User.MustChangePassword {
			t.Error("created user must require password change")
		}
		if len(audit.events) != 1 || audit.events[0].kind != "created" {
			t.Fatalf("audit = %+v, want one created event", audit.events)
		}
		if audit.events[0].role != role || audit.events[0].actor != actor {
			t.Errorf("audit event mismatch: %+v", audit.events[0])
		}
	}
}

func TestCreateUser_AntiEscalation(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	svc := mustService(t, repo, nil)
	for _, r := range []iam.Role{iam.RoleMaster, iam.RoleTenantCommon, iam.Role("is_master"), iam.Role("garbage")} {
		if _, err := svc.CreateUser(context.Background(), uuid.New(), uuid.New(), "a@b.com", r); !errors.Is(err, tenantusers.ErrRoleNotAssignable) {
			t.Errorf("CreateUser(role=%q) err = %v, want ErrRoleNotAssignable", r, err)
		}
	}
	// Nothing must have been persisted.
	got, _ := repo.List(context.Background(), uuid.New())
	if len(got) != 0 {
		t.Fatalf("expected no users persisted, got %d", len(got))
	}
}

func TestCreateUser_EmailConflict(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	repo := newFakeRepo()
	svc := mustService(t, repo, nil)
	if _, err := svc.CreateUser(context.Background(), tenant, uuid.New(), "dup@acme.com", iam.RoleTenantAtendente); err != nil {
		t.Fatalf("first create err = %v", err)
	}
	if _, err := svc.CreateUser(context.Background(), tenant, uuid.New(), "dup@acme.com", iam.RoleTenantAtendente); !errors.Is(err, tenantusers.ErrEmailConflict) {
		t.Fatalf("second create err = %v, want ErrEmailConflict", err)
	}
}

func TestUpdateUserRole(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()

	t.Run("promote atendente to gerente", func(t *testing.T) {
		t.Parallel()
		repo := newFakeRepo()
		audit := &fakeAuditor{}
		svc := mustService(t, repo, audit)
		u := seedUser(repo, tenant, iam.RoleTenantAtendente, true)
		if err := svc.UpdateUserRole(context.Background(), tenant, uuid.New(), u.ID, iam.RoleTenantGerente); err != nil {
			t.Fatalf("UpdateUserRole err = %v", err)
		}
		got, _ := repo.Get(context.Background(), tenant, u.ID)
		if got.Role != iam.RoleTenantGerente {
			t.Errorf("role = %q, want gerente", got.Role)
		}
		if len(audit.events) != 1 || audit.events[0].kind != "role_changed" || audit.events[0].to != iam.RoleTenantGerente {
			t.Fatalf("audit = %+v", audit.events)
		}
	})

	t.Run("no-op change emits no audit", func(t *testing.T) {
		t.Parallel()
		repo := newFakeRepo()
		audit := &fakeAuditor{}
		svc := mustService(t, repo, audit)
		u := seedUser(repo, tenant, iam.RoleTenantAtendente, true)
		if err := svc.UpdateUserRole(context.Background(), tenant, uuid.New(), u.ID, iam.RoleTenantAtendente); err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(audit.events) != 0 {
			t.Fatalf("expected no audit for no-op, got %+v", audit.events)
		}
	})

	t.Run("anti-escalation rejects non-assignable", func(t *testing.T) {
		t.Parallel()
		repo := newFakeRepo()
		svc := mustService(t, repo, nil)
		u := seedUser(repo, tenant, iam.RoleTenantAtendente, true)
		if err := svc.UpdateUserRole(context.Background(), tenant, uuid.New(), u.ID, iam.RoleMaster); !errors.Is(err, tenantusers.ErrRoleNotAssignable) {
			t.Fatalf("err = %v, want ErrRoleNotAssignable", err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		repo := newFakeRepo()
		svc := mustService(t, repo, nil)
		if err := svc.UpdateUserRole(context.Background(), tenant, uuid.New(), uuid.New(), iam.RoleTenantGerente); !errors.Is(err, tenantusers.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("cannot demote last active gerente", func(t *testing.T) {
		t.Parallel()
		repo := newFakeRepo()
		svc := mustService(t, repo, nil)
		g := seedUser(repo, tenant, iam.RoleTenantGerente, true)
		if err := svc.UpdateUserRole(context.Background(), tenant, uuid.New(), g.ID, iam.RoleTenantAtendente); !errors.Is(err, tenantusers.ErrLastGerente) {
			t.Fatalf("err = %v, want ErrLastGerente", err)
		}
	})

	t.Run("can demote gerente when another active gerente remains", func(t *testing.T) {
		t.Parallel()
		repo := newFakeRepo()
		svc := mustService(t, repo, nil)
		g1 := seedUser(repo, tenant, iam.RoleTenantGerente, true)
		seedUser(repo, tenant, iam.RoleTenantGerente, true)
		if err := svc.UpdateUserRole(context.Background(), tenant, uuid.New(), g1.ID, iam.RoleTenantAtendente); err != nil {
			t.Fatalf("err = %v, want nil (second gerente remains)", err)
		}
	})
}

func TestDeactivateUser(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()

	t.Run("deactivate atendente", func(t *testing.T) {
		t.Parallel()
		repo := newFakeRepo()
		audit := &fakeAuditor{}
		svc := mustService(t, repo, audit)
		u := seedUser(repo, tenant, iam.RoleTenantAtendente, true)
		if err := svc.DeactivateUser(context.Background(), tenant, uuid.New(), u.ID); err != nil {
			t.Fatalf("err = %v", err)
		}
		got, _ := repo.Get(context.Background(), tenant, u.ID)
		if got.Active {
			t.Error("user should be inactive")
		}
		if len(audit.events) != 1 || audit.events[0].kind != "deactivated" {
			t.Fatalf("audit = %+v", audit.events)
		}
	})

	t.Run("already inactive is idempotent no-op", func(t *testing.T) {
		t.Parallel()
		repo := newFakeRepo()
		audit := &fakeAuditor{}
		svc := mustService(t, repo, audit)
		u := seedUser(repo, tenant, iam.RoleTenantAtendente, false)
		if err := svc.DeactivateUser(context.Background(), tenant, uuid.New(), u.ID); err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(audit.events) != 0 {
			t.Fatalf("expected no audit for no-op, got %+v", audit.events)
		}
	})

	t.Run("cannot deactivate last active gerente", func(t *testing.T) {
		t.Parallel()
		repo := newFakeRepo()
		svc := mustService(t, repo, nil)
		g := seedUser(repo, tenant, iam.RoleTenantGerente, true)
		if err := svc.DeactivateUser(context.Background(), tenant, uuid.New(), g.ID); !errors.Is(err, tenantusers.ErrLastGerente) {
			t.Fatalf("err = %v, want ErrLastGerente", err)
		}
	})

	t.Run("can deactivate gerente when another remains", func(t *testing.T) {
		t.Parallel()
		repo := newFakeRepo()
		svc := mustService(t, repo, nil)
		g1 := seedUser(repo, tenant, iam.RoleTenantGerente, true)
		seedUser(repo, tenant, iam.RoleTenantGerente, true)
		if err := svc.DeactivateUser(context.Background(), tenant, uuid.New(), g1.ID); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		repo := newFakeRepo()
		svc := mustService(t, repo, nil)
		if err := svc.DeactivateUser(context.Background(), tenant, uuid.New(), uuid.New()); !errors.Is(err, tenantusers.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestListUsers_TenantScoped(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	svc := mustService(t, repo, nil)
	a, b := uuid.New(), uuid.New()
	seedUser(repo, a, iam.RoleTenantGerente, true)
	seedUser(repo, a, iam.RoleTenantAtendente, true)
	seedUser(repo, b, iam.RoleTenantGerente, true)
	got, err := svc.ListUsers(context.Background(), a)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("tenant A users = %d, want 2 (no cross-tenant leak)", len(got))
	}
}

// seedUser inserts a hydrated user into the fake repo and returns it.
func seedUser(repo *fakeRepo, tenant uuid.UUID, role iam.Role, active bool) *tenantusers.User {
	id := uuid.New()
	u := tenantusers.Hydrate(id, tenant, id.String()+"@acme.com", role, active, false, time.Now().UTC())
	repo.seed(u)
	return u
}
