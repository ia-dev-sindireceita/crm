package tenantusers_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/tenantusers"
)

// fakeRepo is an in-memory Repository. It is NOT a database mock for storage
// code — it stands in for the port so the use-case logic (role allowlist,
// audit emission, token minting) is unit-tested without a DB. The adapter's
// storage behaviour (isolation, anti-lockout SQL) is covered separately by
// the postgres integration tests.
type fakeRepo struct {
	users      map[uuid.UUID]tenantusers.User
	createErr  error
	updateErr  error
	deactErr   error
	reactErr   error
	lastCreate *tenantusers.User
	lastToken  tenantusers.Token
	lastPWHash string
}

func newFakeRepo() *fakeRepo { return &fakeRepo{users: map[uuid.UUID]tenantusers.User{}} }

func (f *fakeRepo) List(_ context.Context, tenantID uuid.UUID) ([]tenantusers.User, error) {
	var out []tenantusers.User
	for _, u := range f.users {
		if u.TenantID == tenantID {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *fakeRepo) Get(_ context.Context, tenantID, id uuid.UUID) (tenantusers.User, error) {
	u, ok := f.users[id]
	if !ok || u.TenantID != tenantID {
		return tenantusers.User{}, tenantusers.ErrUserNotFound
	}
	return u, nil
}

func (f *fakeRepo) Create(_ context.Context, u tenantusers.User, passwordHash string, tok tenantusers.Token) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.users[u.ID] = u
	cp := u
	f.lastCreate = &cp
	f.lastToken = tok
	f.lastPWHash = passwordHash
	return nil
}

func (f *fakeRepo) UpdateRole(_ context.Context, tenantID, id uuid.UUID, newRole iam.Role) (iam.Role, error) {
	if f.updateErr != nil {
		return "", f.updateErr
	}
	u, ok := f.users[id]
	if !ok || u.TenantID != tenantID {
		return "", tenantusers.ErrUserNotFound
	}
	before := u.Role
	u.Role = newRole
	f.users[id] = u
	return before, nil
}

func (f *fakeRepo) Deactivate(_ context.Context, tenantID, id uuid.UUID) (iam.Role, error) {
	if f.deactErr != nil {
		return "", f.deactErr
	}
	u, ok := f.users[id]
	if !ok || u.TenantID != tenantID {
		return "", tenantusers.ErrUserNotFound
	}
	t := time.Unix(0, 0)
	u.DeactivatedAt = &t
	f.users[id] = u
	return u.Role, nil
}

func (f *fakeRepo) Reactivate(_ context.Context, tenantID, id uuid.UUID) (iam.Role, error) {
	if f.reactErr != nil {
		return "", f.reactErr
	}
	u, ok := f.users[id]
	if !ok || u.TenantID != tenantID {
		return "", tenantusers.ErrUserNotFound
	}
	u.DeactivatedAt = nil
	f.users[id] = u
	return u.Role, nil
}

// fakeHasher records the plaintext it was asked to hash.
type fakeHasher struct{ last string }

func (h *fakeHasher) Hash(plain string) (string, error) {
	h.last = plain
	return "argon2id$" + plain, nil
}

// fakeAuditor records security events.
type fakeAuditor struct{ events []audit.SecurityAuditEvent }

func (a *fakeAuditor) WriteSecurity(_ context.Context, ev audit.SecurityAuditEvent) error {
	a.events = append(a.events, ev)
	return nil
}

func newService(t *testing.T, repo tenantusers.Repository) (*tenantusers.Service, *fakeAuditor) {
	t.Helper()
	aud := &fakeAuditor{}
	// Deterministic rand: fill with a fixed byte so token/password are stable.
	randn := func(b []byte) (int, error) {
		for i := range b {
			b[i] = 7
		}
		return len(b), nil
	}
	svc, err := tenantusers.NewService(tenantusers.Config{
		Repo:    repo,
		Hasher:  &fakeHasher{},
		Auditor: aud,
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Rand:    randn,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, aud
}

func TestNewService_RequiresDeps(t *testing.T) {
	t.Parallel()
	_, err := tenantusers.NewService(tenantusers.Config{})
	if err == nil {
		t.Fatal("want error for nil Repo")
	}
}

func TestCreate_AssignsBothRoles(t *testing.T) {
	t.Parallel()
	for _, role := range []iam.Role{iam.RoleTenantAtendente, iam.RoleTenantGerente} {
		repo := newFakeRepo()
		svc, aud := newService(t, repo)
		actor := tenantusers.Actor{TenantID: uuid.New(), UserID: uuid.New()}
		u, tok, err := svc.Create(context.Background(), actor, "new@tenant.example", role)
		if err != nil {
			t.Fatalf("Create(%s): %v", role, err)
		}
		if u.Role != role {
			t.Fatalf("role = %s, want %s", u.Role, role)
		}
		if u.TenantID != actor.TenantID {
			t.Fatalf("tenant not taken from actor: got %s", u.TenantID)
		}
		if tok.Plaintext == "" || len(tok.SHA256) == 0 {
			t.Fatal("expected an invite token with plaintext + hash")
		}
		if repo.lastPWHash == "" {
			t.Fatal("expected a random placeholder password hash to be stored")
		}
		// Audit: a user_create event with the role, and no secret leakage.
		if len(aud.events) != 1 || aud.events[0].Event != audit.SecurityEventUserCreate {
			t.Fatalf("want one user_create audit, got %+v", aud.events)
		}
		if got := aud.events[0].Target["role"]; got != string(role) {
			t.Fatalf("audit role = %v, want %s", got, role)
		}
		for k := range aud.events[0].Target {
			if strings.Contains(strings.ToLower(k), "token") || strings.Contains(strings.ToLower(k), "password") {
				t.Fatalf("audit target leaked a credential field: %s", k)
			}
		}
	}
}

func TestCreate_RejectsMasterAndArbitraryRoles(t *testing.T) {
	t.Parallel()
	for _, role := range []iam.Role{iam.RoleMaster, iam.RoleTenantCommon, iam.RoleTenantLider, iam.Role("admin"), iam.Role("")} {
		repo := newFakeRepo()
		svc, aud := newService(t, repo)
		actor := tenantusers.Actor{TenantID: uuid.New(), UserID: uuid.New()}
		_, _, err := svc.Create(context.Background(), actor, "x@tenant.example", role)
		if !errors.Is(err, tenantusers.ErrInvalidRole) {
			t.Fatalf("role %q: err = %v, want ErrInvalidRole", role, err)
		}
		if len(repo.users) != 0 {
			t.Fatalf("role %q: user was created despite invalid role", role)
		}
		if len(aud.events) != 0 {
			t.Fatalf("role %q: audit written for rejected create", role)
		}
	}
}

func TestCreate_RejectsInvalidEmail(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t, newFakeRepo())
	actor := tenantusers.Actor{TenantID: uuid.New(), UserID: uuid.New()}
	for _, email := range []string{"", "   ", "not-an-email", "a@b <a@b>", "Name <a@b.com>"} {
		if _, _, err := svc.Create(context.Background(), actor, email, iam.RoleTenantAtendente); !errors.Is(err, tenantusers.ErrInvalidEmail) {
			t.Fatalf("email %q: err = %v, want ErrInvalidEmail", email, err)
		}
	}
}

func TestCreate_PropagatesEmailTaken(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.createErr = tenantusers.ErrEmailTaken
	svc, _ := newService(t, repo)
	actor := tenantusers.Actor{TenantID: uuid.New(), UserID: uuid.New()}
	if _, _, err := svc.Create(context.Background(), actor, "dup@tenant.example", iam.RoleTenantAtendente); !errors.Is(err, tenantusers.ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}
}

func TestUpdateRole_RejectsInvalidRole(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t, newFakeRepo())
	actor := tenantusers.Actor{TenantID: uuid.New(), UserID: uuid.New()}
	if _, err := svc.UpdateRole(context.Background(), actor, uuid.New(), iam.RoleMaster); !errors.Is(err, tenantusers.ErrInvalidRole) {
		t.Fatalf("err = %v, want ErrInvalidRole", err)
	}
}

func TestUpdateRole_AuditsRoleChange(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	actor := tenantusers.Actor{TenantID: uuid.New(), UserID: uuid.New()}
	id := uuid.New()
	repo.users[id] = tenantusers.User{ID: id, TenantID: actor.TenantID, Email: "u@t.example", Role: iam.RoleTenantAtendente}
	svc, aud := newService(t, repo)
	if _, err := svc.UpdateRole(context.Background(), actor, id, iam.RoleTenantGerente); err != nil {
		t.Fatalf("UpdateRole: %v", err)
	}
	if len(aud.events) != 1 || aud.events[0].Event != audit.SecurityEventRoleChange {
		t.Fatalf("want role_change audit, got %+v", aud.events)
	}
	if aud.events[0].Target["before_role"] != "tenant_atendente" || aud.events[0].Target["after_role"] != "tenant_gerente" {
		t.Fatalf("audit before/after wrong: %+v", aud.events[0].Target)
	}
}

func TestUpdateRole_PropagatesLastGerente(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.updateErr = tenantusers.ErrLastGerente
	svc, aud := newService(t, repo)
	actor := tenantusers.Actor{TenantID: uuid.New(), UserID: uuid.New()}
	if _, err := svc.UpdateRole(context.Background(), actor, uuid.New(), iam.RoleTenantAtendente); !errors.Is(err, tenantusers.ErrLastGerente) {
		t.Fatalf("err = %v, want ErrLastGerente", err)
	}
	if len(aud.events) != 0 {
		t.Fatal("no audit should be written on a blocked role change")
	}
}

func TestDeactivate_AuditsAndPropagatesLastGerente(t *testing.T) {
	t.Parallel()
	// happy path
	repo := newFakeRepo()
	actor := tenantusers.Actor{TenantID: uuid.New(), UserID: uuid.New()}
	id := uuid.New()
	repo.users[id] = tenantusers.User{ID: id, TenantID: actor.TenantID, Role: iam.RoleTenantAtendente}
	svc, aud := newService(t, repo)
	if _, err := svc.Deactivate(context.Background(), actor, id); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if len(aud.events) != 1 || aud.events[0].Event != audit.SecurityEventUserDeactivate {
		t.Fatalf("want user_deactivate audit, got %+v", aud.events)
	}

	// blocked path
	repo2 := newFakeRepo()
	repo2.deactErr = tenantusers.ErrLastGerente
	svc2, aud2 := newService(t, repo2)
	if _, err := svc2.Deactivate(context.Background(), actor, uuid.New()); !errors.Is(err, tenantusers.ErrLastGerente) {
		t.Fatalf("err = %v, want ErrLastGerente", err)
	}
	if len(aud2.events) != 0 {
		t.Fatal("no audit on blocked deactivate")
	}
}

func TestReactivate_Audits(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	actor := tenantusers.Actor{TenantID: uuid.New(), UserID: uuid.New()}
	id := uuid.New()
	past := time.Unix(0, 0)
	repo.users[id] = tenantusers.User{ID: id, TenantID: actor.TenantID, Role: iam.RoleTenantAtendente, DeactivatedAt: &past}
	svc, aud := newService(t, repo)
	u, err := svc.Reactivate(context.Background(), actor, id)
	if err != nil {
		t.Fatalf("Reactivate: %v", err)
	}
	if !u.Active() {
		t.Fatal("user should be active after reactivate")
	}
	if len(aud.events) != 1 || aud.events[0].Event != audit.SecurityEventUserReactivate {
		t.Fatalf("want user_reactivate audit, got %+v", aud.events)
	}
}

func TestList_ScopesToActorTenant(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	mine := uuid.New()
	other := uuid.New()
	repo.users[uuid.New()] = tenantusers.User{ID: uuid.New(), TenantID: mine, Role: iam.RoleTenantGerente}
	repo.users[uuid.New()] = tenantusers.User{ID: uuid.New(), TenantID: other, Role: iam.RoleTenantGerente}
	svc, _ := newService(t, repo)
	got, err := svc.List(context.Background(), tenantusers.Actor{TenantID: mine})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].TenantID != mine {
		t.Fatalf("List leaked cross-tenant rows: %+v", got)
	}
}

func TestGenerateToken_HashMatchesPlaintext(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	tok, err := tenantusers.GenerateToken(func(b []byte) (int, error) {
		for i := range b {
			b[i] = byte(i)
		}
		return len(b), nil
	}, tenantusers.PurposeInvite, now, tenantusers.InviteTTL)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if tok.Plaintext == "" {
		t.Fatal("empty plaintext")
	}
	want := tenantusers.HashToken(tok.Plaintext)
	if string(want) != string(tok.SHA256) {
		t.Fatal("stored hash does not match SHA-256 of plaintext")
	}
	if !tok.ExpiresAt.Equal(now.Add(tenantusers.InviteTTL)) {
		t.Fatalf("expiry = %v, want now+TTL", tok.ExpiresAt)
	}
}
