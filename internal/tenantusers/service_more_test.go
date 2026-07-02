package tenantusers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/tenantusers"
)

func TestAssignableRoles(t *testing.T) {
	t.Parallel()
	got := tenantusers.AssignableRoles()
	if len(got) != 2 {
		t.Fatalf("AssignableRoles len = %d, want 2", len(got))
	}
	if !tenantusers.AssignableRole(iam.RoleTenantGerente) || !tenantusers.AssignableRole(iam.RoleTenantAtendente) {
		t.Fatal("gerente and atendente must be assignable")
	}
	for _, r := range []iam.Role{iam.RoleMaster, iam.RoleTenantCommon, iam.RoleTenantLider, iam.Role("x")} {
		if tenantusers.AssignableRole(r) {
			t.Fatalf("role %q must not be assignable", r)
		}
	}
}

// errAuditor fails every write; the mutation must still succeed (audit is
// best-effort, logged not fatal).
type errAuditor struct{}

func (errAuditor) WriteSecurity(context.Context, audit.SecurityAuditEvent) error {
	return errors.New("audit down")
}

func TestCreate_SucceedsWhenAuditFails(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	svc, err := tenantusers.NewService(tenantusers.Config{
		Repo:    repo,
		Hasher:  &fakeHasher{},
		Auditor: errAuditor{},
		Now:     func() time.Time { return time.Unix(1, 0).UTC() },
		Rand:    func(b []byte) (int, error) { return len(b), nil },
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	actor := tenantusers.Actor{TenantID: uuid.New(), UserID: uuid.New()}
	if _, _, err := svc.Create(context.Background(), actor, "a@b.example", iam.RoleTenantAtendente); err != nil {
		t.Fatalf("Create should succeed despite audit failure: %v", err)
	}
	if len(repo.users) != 1 {
		t.Fatal("user should be persisted even when audit fails")
	}
}

// failRand always errors — Create must surface the failure before persisting.
func failRand([]byte) (int, error) { return 0, errors.New("no entropy") }

func TestCreate_RandFailurePropagates(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	svc, err := tenantusers.NewService(tenantusers.Config{
		Repo:    repo,
		Hasher:  &fakeHasher{},
		Auditor: &fakeAuditor{},
		Rand:    failRand,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	actor := tenantusers.Actor{TenantID: uuid.New(), UserID: uuid.New()}
	if _, _, err := svc.Create(context.Background(), actor, "a@b.example", iam.RoleTenantAtendente); err == nil {
		t.Fatal("want error when rand fails")
	}
	if len(repo.users) != 0 {
		t.Fatal("no user should be persisted when entropy fails")
	}
}

func TestGenerateToken_RandFailure(t *testing.T) {
	t.Parallel()
	if _, err := tenantusers.GenerateToken(failRand, tenantusers.PurposeInvite, time.Unix(0, 0), tenantusers.InviteTTL); err == nil {
		t.Fatal("want error when rand fails")
	}
}

func TestUpdateRole_Get(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	actor := tenantusers.Actor{TenantID: uuid.New(), UserID: uuid.New()}
	id := uuid.New()
	repo.users[id] = tenantusers.User{ID: id, TenantID: actor.TenantID, Role: iam.RoleTenantAtendente}
	svc, _ := newService(t, repo)
	u, err := svc.UpdateRole(context.Background(), actor, id, iam.RoleTenantGerente)
	if err != nil {
		t.Fatalf("UpdateRole: %v", err)
	}
	if u.Role != iam.RoleTenantGerente {
		t.Fatalf("returned role = %s, want gerente", u.Role)
	}
}

func TestGet_Passthrough(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	actor := tenantusers.Actor{TenantID: uuid.New(), UserID: uuid.New()}
	id := uuid.New()
	repo.users[id] = tenantusers.User{ID: id, TenantID: actor.TenantID, Email: "g@t.example", Role: iam.RoleTenantGerente}
	svc, _ := newService(t, repo)
	u, err := svc.Get(context.Background(), actor, id)
	if err != nil || u.Email != "g@t.example" {
		t.Fatalf("Get = (%+v, %v)", u, err)
	}
}
