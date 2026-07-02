package tenantusers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/iam/password"
	"github.com/pericles-luz/crm/internal/tenantusers"
)

// fakeCredRepo is an in-memory CredentialTokenRepository. It stands in for the
// storage port so the use-case logic (hash lookup, policy gate ordering,
// atomic-consume delegation, audit emission) is unit-tested without a DB. The
// adapter's real single-use / RLS behaviour is covered by the postgres
// integration tests.
type fakeCredRepo struct {
	// keyed by hex(hash) so a []byte lookup works as a map key.
	invites     map[string]tenantusers.Invite
	lookupErr   error
	consumeErr  error
	consumed    map[string]bool
	lastConsume struct {
		hash []byte
		now  time.Time
		enc  string
	}
}

func newFakeCredRepo() *fakeCredRepo {
	return &fakeCredRepo{invites: map[string]tenantusers.Invite{}, consumed: map[string]bool{}}
}

func hkey(b []byte) string { return string(b) }

func (f *fakeCredRepo) LookupToken(_ context.Context, _ uuid.UUID, tokenHash []byte, _ time.Time) (tenantusers.Invite, error) {
	if f.lookupErr != nil {
		return tenantusers.Invite{}, f.lookupErr
	}
	k := hkey(tokenHash)
	if f.consumed[k] {
		return tenantusers.Invite{}, tenantusers.ErrTokenInvalid
	}
	inv, ok := f.invites[k]
	if !ok {
		return tenantusers.Invite{}, tenantusers.ErrTokenInvalid
	}
	return inv, nil
}

func (f *fakeCredRepo) ConsumeToken(_ context.Context, _ uuid.UUID, tokenHash []byte, now time.Time, enc string) (uuid.UUID, error) {
	if f.consumeErr != nil {
		return uuid.Nil, f.consumeErr
	}
	k := hkey(tokenHash)
	if f.consumed[k] {
		return uuid.Nil, tenantusers.ErrTokenInvalid
	}
	inv, ok := f.invites[k]
	if !ok {
		return uuid.Nil, tenantusers.ErrTokenInvalid
	}
	f.consumed[k] = true
	f.lastConsume.hash = tokenHash
	f.lastConsume.now = now
	f.lastConsume.enc = enc
	return inv.UserID, nil
}

// fakePolicy records the plaintext/context it saw and returns a configurable
// error.
type fakePolicy struct {
	err      error
	lastPln  string
	lastPctx password.PolicyContext
}

func (p *fakePolicy) PolicyCheck(_ context.Context, plain string, pctx password.PolicyContext) error {
	p.lastPln = plain
	p.lastPctx = pctx
	return p.err
}

func newCredService(t *testing.T, repo tenantusers.CredentialTokenRepository, pol tenantusers.PasswordPolicy) (*tenantusers.CredentialService, *fakeHasher, *fakeAuditor) {
	t.Helper()
	h := &fakeHasher{}
	aud := &fakeAuditor{}
	svc, err := tenantusers.NewCredentialService(tenantusers.CredentialConfig{
		Repo:    repo,
		Hasher:  h,
		Policy:  pol,
		Auditor: aud,
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})
	if err != nil {
		t.Fatalf("NewCredentialService: %v", err)
	}
	return svc, h, aud
}

func TestNewCredentialService_RequiredDeps(t *testing.T) {
	base := func() tenantusers.CredentialConfig {
		return tenantusers.CredentialConfig{
			Repo:    newFakeCredRepo(),
			Hasher:  &fakeHasher{},
			Policy:  &fakePolicy{},
			Auditor: &fakeAuditor{},
		}
	}
	tests := map[string]func(c *tenantusers.CredentialConfig){
		"nil repo":    func(c *tenantusers.CredentialConfig) { c.Repo = nil },
		"nil hasher":  func(c *tenantusers.CredentialConfig) { c.Hasher = nil },
		"nil policy":  func(c *tenantusers.CredentialConfig) { c.Policy = nil },
		"nil auditor": func(c *tenantusers.CredentialConfig) { c.Auditor = nil },
	}
	for name, mut := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			mut(&cfg)
			if _, err := tenantusers.NewCredentialService(cfg); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}

	// All deps present → ok, and Now/Logger default.
	if _, err := tenantusers.NewCredentialService(base()); err != nil {
		t.Fatalf("valid config: %v", err)
	}
}

func seedInvite(repo *fakeCredRepo, plain string, inv tenantusers.Invite) {
	repo.invites[hkey(tenantusers.HashToken(plain))] = inv
}

func TestResolve(t *testing.T) {
	repo := newFakeCredRepo()
	tenant := uuid.New()
	want := tenantusers.Invite{UserID: uuid.New(), Email: "user@ex.com", Purpose: tenantusers.PurposeInvite}
	seedInvite(repo, "good-token", want)
	svc, _, _ := newCredService(t, repo, &fakePolicy{})

	got, err := svc.Resolve(context.Background(), tenant, "good-token")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// Empty token → ErrTokenInvalid, no repo call.
	if _, err := svc.Resolve(context.Background(), tenant, ""); !errors.Is(err, tenantusers.ErrTokenInvalid) {
		t.Fatalf("empty token err=%v, want ErrTokenInvalid", err)
	}
	// Unknown token → ErrTokenInvalid.
	if _, err := svc.Resolve(context.Background(), tenant, "nope"); !errors.Is(err, tenantusers.ErrTokenInvalid) {
		t.Fatalf("unknown token err=%v, want ErrTokenInvalid", err)
	}
}

func TestSetPassword_HappyPath(t *testing.T) {
	repo := newFakeCredRepo()
	tenant := uuid.New()
	uid := uuid.New()
	seedInvite(repo, "tok", tenantusers.Invite{UserID: uid, Email: "u@ex.com", Purpose: tenantusers.PurposeInvite})
	pol := &fakePolicy{}
	svc, h, aud := newCredService(t, repo, pol)

	inv, err := svc.SetPassword(context.Background(), tenant, "tok", "Str0ng-Passphrase!", password.PolicyContext{TenantName: "Acme"})
	if err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if inv.UserID != uid || inv.Email != "u@ex.com" {
		t.Fatalf("returned invite %+v", inv)
	}
	// Policy saw the resolved email (identity rule cannot be spoofed) and the
	// caller-supplied tenant name.
	if pol.lastPctx.Email != "u@ex.com" || pol.lastPctx.TenantName != "Acme" {
		t.Fatalf("policy pctx=%+v", pol.lastPctx)
	}
	// The accepted password (not the token) is what got hashed and stored.
	if h.last != "Str0ng-Passphrase!" {
		t.Fatalf("hashed %q", h.last)
	}
	if repo.lastConsume.enc != "argon2id$Str0ng-Passphrase!" {
		t.Fatalf("consumed enc=%q", repo.lastConsume.enc)
	}
	// Token is now single-use spent.
	if !repo.consumed[hkey(tenantusers.HashToken("tok"))] {
		t.Fatalf("token not marked consumed")
	}
	// Exactly one password_reset audit event, self-actored, tenant-scoped.
	if len(aud.events) != 1 {
		t.Fatalf("audit events=%d, want 1", len(aud.events))
	}
	ev := aud.events[0]
	if ev.Event != audit.SecurityEventPasswordReset {
		t.Fatalf("event=%q", ev.Event)
	}
	if ev.ActorUserID != uid {
		t.Fatalf("actor=%v want self %v", ev.ActorUserID, uid)
	}
	if ev.TenantID == nil || *ev.TenantID != tenant {
		t.Fatalf("tenant scope=%v", ev.TenantID)
	}
	if ev.Target["via"] != "credential_token" || ev.Target["purpose"] != "invite" {
		t.Fatalf("target=%+v", ev.Target)
	}
}

func TestSetPassword_PolicyFailure_DoesNotConsume(t *testing.T) {
	repo := newFakeCredRepo()
	tenant := uuid.New()
	seedInvite(repo, "tok", tenantusers.Invite{UserID: uuid.New(), Email: "u@ex.com", Purpose: tenantusers.PurposeInvite})
	polErr := &password.PolicyError{Reason: password.ReasonTooShort, Detail: "min 12"}
	svc, h, aud := newCredService(t, repo, &fakePolicy{err: polErr})

	_, err := svc.SetPassword(context.Background(), tenant, "tok", "short", password.PolicyContext{})
	var perr *password.PolicyError
	if !errors.As(err, &perr) || perr.Reason != password.ReasonTooShort {
		t.Fatalf("err=%v, want PolicyError too_short", err)
	}
	// Token NOT consumed, nothing hashed, no audit.
	if repo.consumed[hkey(tenantusers.HashToken("tok"))] {
		t.Fatalf("token was consumed on policy failure")
	}
	if h.last != "" {
		t.Fatalf("password hashed despite policy failure")
	}
	if len(aud.events) != 0 {
		t.Fatalf("audit written despite policy failure")
	}
}

func TestSetPassword_InvalidToken(t *testing.T) {
	repo := newFakeCredRepo()
	svc, _, aud := newCredService(t, repo, &fakePolicy{})

	// Empty token short-circuits.
	if _, err := svc.SetPassword(context.Background(), uuid.New(), "", "whatever-long-pass", password.PolicyContext{}); !errors.Is(err, tenantusers.ErrTokenInvalid) {
		t.Fatalf("empty: err=%v", err)
	}
	// Unknown token → ErrTokenInvalid from lookup.
	if _, err := svc.SetPassword(context.Background(), uuid.New(), "ghost", "whatever-long-pass", password.PolicyContext{}); !errors.Is(err, tenantusers.ErrTokenInvalid) {
		t.Fatalf("unknown: err=%v", err)
	}
	if len(aud.events) != 0 {
		t.Fatalf("audit written for invalid token")
	}
}

func TestSetPassword_ConsumeRaceLost(t *testing.T) {
	repo := newFakeCredRepo()
	tenant := uuid.New()
	seedInvite(repo, "tok", tenantusers.Invite{UserID: uuid.New(), Email: "u@ex.com", Purpose: tenantusers.PurposeInvite})
	// Lookup passes, policy passes, but the atomic consume loses the race.
	repo.consumeErr = tenantusers.ErrTokenInvalid
	svc, _, aud := newCredService(t, repo, &fakePolicy{})

	if _, err := svc.SetPassword(context.Background(), tenant, "tok", "Str0ng-Passphrase!", password.PolicyContext{}); !errors.Is(err, tenantusers.ErrTokenInvalid) {
		t.Fatalf("err=%v, want ErrTokenInvalid", err)
	}
	if len(aud.events) != 0 {
		t.Fatalf("audit written when consume lost the race")
	}
}

func TestSetPassword_HashError(t *testing.T) {
	repo := newFakeCredRepo()
	tenant := uuid.New()
	seedInvite(repo, "tok", tenantusers.Invite{UserID: uuid.New(), Email: "u@ex.com", Purpose: tenantusers.PurposeInvite})
	svc, _, _ := newCredServiceWithHasher(t, repo, &fakePolicy{}, errHasher{})

	if _, err := svc.SetPassword(context.Background(), tenant, "tok", "Str0ng-Passphrase!", password.PolicyContext{}); err == nil {
		t.Fatalf("expected hash error")
	}
	if repo.consumed[hkey(tenantusers.HashToken("tok"))] {
		t.Fatalf("token consumed despite hash failure")
	}
}

// errHasher always fails.
type errHasher struct{}

func (errHasher) Hash(string) (string, error) { return "", errors.New("boom") }

func newCredServiceWithHasher(t *testing.T, repo tenantusers.CredentialTokenRepository, pol tenantusers.PasswordPolicy, h tenantusers.Hasher) (*tenantusers.CredentialService, tenantusers.Hasher, *fakeAuditor) {
	t.Helper()
	aud := &fakeAuditor{}
	svc, err := tenantusers.NewCredentialService(tenantusers.CredentialConfig{
		Repo:    repo,
		Hasher:  h,
		Policy:  pol,
		Auditor: aud,
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})
	if err != nil {
		t.Fatalf("NewCredentialService: %v", err)
	}
	return svc, h, aud
}
