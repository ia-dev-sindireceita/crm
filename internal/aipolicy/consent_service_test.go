package aipolicy_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aipolicy"
)

// fakeConsentRepo is a hand-rolled concurrent-safe in-memory
// ConsentRepository for the service unit tests. The integration test
// against Postgres lives in internal/adapter/db/postgres so the
// service tests can run without a database.
type fakeConsentRepo struct {
	mu        sync.Mutex
	rows      map[string]aipolicy.Consent
	getErr    error
	upsertErr error
	getCalls  int
	upserts   int
}

func newFakeConsentRepo() *fakeConsentRepo {
	return &fakeConsentRepo{rows: map[string]aipolicy.Consent{}}
}

func consentKey(tenantID uuid.UUID, kind aipolicy.ScopeType, scopeID string) string {
	return tenantID.String() + "|" + string(kind) + "|" + scopeID
}

func (f *fakeConsentRepo) Get(_ context.Context, tenantID uuid.UUID, kind aipolicy.ScopeType, scopeID string) (aipolicy.Consent, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return aipolicy.Consent{}, false, f.getErr
	}
	c, ok := f.rows[consentKey(tenantID, kind, scopeID)]
	if !ok {
		return aipolicy.Consent{}, false, nil
	}
	return c, true, nil
}

func (f *fakeConsentRepo) Upsert(_ context.Context, c aipolicy.Consent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts++
	if f.upsertErr != nil {
		return f.upsertErr
	}
	c.AcceptedAt = time.Now()
	f.rows[consentKey(c.TenantID, c.ScopeKind, c.ScopeID)] = c
	return nil
}

func tenantScope(tenantID uuid.UUID) aipolicy.ConsentScope {
	return aipolicy.ConsentScope{TenantID: tenantID, Kind: aipolicy.ScopeTenant, ID: tenantID.String()}
}

func TestNewConsentService_RejectsNilRepo(t *testing.T) {
	t.Parallel()
	_, err := aipolicy.NewConsentService(nil)
	if !errors.Is(err, aipolicy.ErrNilConsentRepository) {
		t.Fatalf("got err=%v; want ErrNilConsentRepository", err)
	}
}

func TestNewConsentService_AcceptsValidRepo(t *testing.T) {
	t.Parallel()
	svc, err := aipolicy.NewConsentService(newFakeConsentRepo())
	if err != nil {
		t.Fatalf("NewConsentService: %v", err)
	}
	if svc == nil {
		t.Fatal("service is nil")
	}
}

func TestHasConsent_NoRowReturnsFalse(t *testing.T) {
	t.Parallel()
	repo := newFakeConsentRepo()
	svc, _ := aipolicy.NewConsentService(repo)
	tenantID := uuid.New()

	has, err := svc.HasConsent(context.Background(), tenantScope(tenantID), "anon-v1", "prompt-v1")
	if err != nil {
		t.Fatalf("HasConsent: %v", err)
	}
	if has {
		t.Errorf("HasConsent without row = true; want false")
	}
}

func TestHasConsent_MatchingRowReturnsTrue(t *testing.T) {
	t.Parallel()
	repo := newFakeConsentRepo()
	svc, _ := aipolicy.NewConsentService(repo)
	tenantID := uuid.New()
	scope := tenantScope(tenantID)

	if err := svc.RecordConsent(context.Background(), scope, nil, "preview text", "anon-v1", "prompt-v1"); err != nil {
		t.Fatalf("seed RecordConsent: %v", err)
	}

	has, err := svc.HasConsent(context.Background(), scope, "anon-v1", "prompt-v1")
	if err != nil {
		t.Fatalf("HasConsent: %v", err)
	}
	if !has {
		t.Errorf("HasConsent on matching row = false; want true")
	}
}

func TestHasConsent_AnonymizerVersionMismatchReturnsFalse(t *testing.T) {
	t.Parallel()
	repo := newFakeConsentRepo()
	svc, _ := aipolicy.NewConsentService(repo)
	tenantID := uuid.New()
	scope := tenantScope(tenantID)

	if err := svc.RecordConsent(context.Background(), scope, nil, "preview", "anon-v1", "prompt-v1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	has, err := svc.HasConsent(context.Background(), scope, "anon-v2", "prompt-v1")
	if err != nil {
		t.Fatalf("HasConsent: %v", err)
	}
	if has {
		t.Errorf("HasConsent under bumped anonymizer = true; want false (re-consent trigger)")
	}
}

func TestHasConsent_PromptVersionMismatchReturnsFalse(t *testing.T) {
	t.Parallel()
	repo := newFakeConsentRepo()
	svc, _ := aipolicy.NewConsentService(repo)
	tenantID := uuid.New()
	scope := tenantScope(tenantID)

	if err := svc.RecordConsent(context.Background(), scope, nil, "preview", "anon-v1", "prompt-v1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	has, err := svc.HasConsent(context.Background(), scope, "anon-v1", "prompt-v2")
	if err != nil {
		t.Fatalf("HasConsent: %v", err)
	}
	if has {
		t.Errorf("HasConsent under bumped prompt = true; want false (re-consent trigger)")
	}
}

func TestHasConsent_BoundaryRejects(t *testing.T) {
	t.Parallel()
	repo := newFakeConsentRepo()
	svc, _ := aipolicy.NewConsentService(repo)
	good := tenantScope(uuid.New())

	cases := []struct {
		name    string
		scope   aipolicy.ConsentScope
		anonVer string
		prmtVer string
		want    error
	}{
		{"zero tenant", aipolicy.ConsentScope{Kind: aipolicy.ScopeTenant, ID: "x"}, "v1", "v1", aipolicy.ErrInvalidTenant},
		{"bad kind", aipolicy.ConsentScope{TenantID: uuid.New(), Kind: "bogus", ID: "x"}, "v1", "v1", aipolicy.ErrInvalidScopeType},
		{"blank id", aipolicy.ConsentScope{TenantID: uuid.New(), Kind: aipolicy.ScopeTenant, ID: "  "}, "v1", "v1", aipolicy.ErrInvalidScopeID},
		{"blank anon", good, " ", "v1", aipolicy.ErrInvalidAnonymizerVersion},
		{"blank prompt", good, "v1", "", aipolicy.ErrInvalidPromptVersion},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := svc.HasConsent(context.Background(), tc.scope, tc.anonVer, tc.prmtVer)
			if !errors.Is(err, tc.want) {
				t.Fatalf("HasConsent(%q): err=%v; want %v", tc.name, err, tc.want)
			}
		})
	}
}

func TestHasConsent_RepoErrorBubbles(t *testing.T) {
	t.Parallel()
	repo := newFakeConsentRepo()
	repoErr := errors.New("transport boom")
	repo.getErr = repoErr
	svc, _ := aipolicy.NewConsentService(repo)

	_, err := svc.HasConsent(context.Background(), tenantScope(uuid.New()), "v1", "v1")
	if !errors.Is(err, repoErr) {
		t.Fatalf("HasConsent: %v; want wrapping repoErr", err)
	}
}

func TestRecordConsent_StoresCorrectHash(t *testing.T) {
	t.Parallel()
	repo := newFakeConsentRepo()
	svc, _ := aipolicy.NewConsentService(repo)
	tenantID := uuid.New()
	scope := tenantScope(tenantID)

	const preview = "anonymized preview"
	if err := svc.RecordConsent(context.Background(), scope, nil, preview, "anon-v1", "prompt-v1"); err != nil {
		t.Fatalf("RecordConsent: %v", err)
	}

	got, ok, err := repo.Get(context.Background(), tenantID, aipolicy.ScopeTenant, tenantID.String())
	if err != nil || !ok {
		t.Fatalf("repo.Get: ok=%v err=%v", ok, err)
	}
	want := sha256.Sum256([]byte(preview))
	if got.PayloadHash != want {
		t.Errorf("stored payload hash mismatch")
	}
	if got.AnonymizerVersion != "anon-v1" {
		t.Errorf("anonymizer version = %q; want anon-v1", got.AnonymizerVersion)
	}
	if got.PromptVersion != "prompt-v1" {
		t.Errorf("prompt version = %q; want prompt-v1", got.PromptVersion)
	}
}

func TestRecordConsent_IdempotentNoOpOnSameTriple(t *testing.T) {
	t.Parallel()
	repo := newFakeConsentRepo()
	svc, _ := aipolicy.NewConsentService(repo)
	tenantID := uuid.New()
	scope := tenantScope(tenantID)

	if err := svc.RecordConsent(context.Background(), scope, nil, "p", "anon-v1", "prompt-v1"); err != nil {
		t.Fatalf("first RecordConsent: %v", err)
	}
	if err := svc.RecordConsent(context.Background(), scope, nil, "p", "anon-v1", "prompt-v1"); err != nil {
		t.Fatalf("repeat RecordConsent: %v", err)
	}
	if repo.upserts != 1 {
		t.Errorf("upserts = %d; want 1 (second call must be no-op)", repo.upserts)
	}
}

func TestRecordConsent_NewHashTriggersUpsert(t *testing.T) {
	t.Parallel()
	repo := newFakeConsentRepo()
	svc, _ := aipolicy.NewConsentService(repo)
	tenantID := uuid.New()
	scope := tenantScope(tenantID)

	if err := svc.RecordConsent(context.Background(), scope, nil, "first", "anon-v1", "prompt-v1"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := svc.RecordConsent(context.Background(), scope, nil, "second", "anon-v1", "prompt-v1"); err != nil {
		t.Fatalf("second: %v", err)
	}
	if repo.upserts != 2 {
		t.Errorf("upserts = %d; want 2 (hash bump must Upsert)", repo.upserts)
	}
}

func TestRecordConsent_VersionBumpTriggersUpsert(t *testing.T) {
	t.Parallel()
	repo := newFakeConsentRepo()
	svc, _ := aipolicy.NewConsentService(repo)
	tenantID := uuid.New()
	scope := tenantScope(tenantID)

	if err := svc.RecordConsent(context.Background(), scope, nil, "p", "anon-v1", "prompt-v1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := svc.RecordConsent(context.Background(), scope, nil, "p", "anon-v2", "prompt-v1"); err != nil {
		t.Fatalf("anon bump: %v", err)
	}
	if err := svc.RecordConsent(context.Background(), scope, nil, "p", "anon-v2", "prompt-v2"); err != nil {
		t.Fatalf("prompt bump: %v", err)
	}
	if repo.upserts != 3 {
		t.Errorf("upserts = %d; want 3 (each version bump must Upsert)", repo.upserts)
	}
}

func TestRecordConsent_PersistsActorUserID(t *testing.T) {
	t.Parallel()
	repo := newFakeConsentRepo()
	svc, _ := aipolicy.NewConsentService(repo)
	tenantID := uuid.New()
	actor := uuid.New()
	scope := tenantScope(tenantID)

	if err := svc.RecordConsent(context.Background(), scope, &actor, "p", "anon-v1", "prompt-v1"); err != nil {
		t.Fatalf("RecordConsent: %v", err)
	}
	row, ok, _ := repo.Get(context.Background(), tenantID, aipolicy.ScopeTenant, tenantID.String())
	if !ok {
		t.Fatal("seed not found")
	}
	if row.ActorUserID == nil || *row.ActorUserID != actor {
		t.Errorf("actor = %v; want %v", row.ActorUserID, actor)
	}
}

func TestRecordConsent_NilActorIsPersisted(t *testing.T) {
	t.Parallel()
	repo := newFakeConsentRepo()
	svc, _ := aipolicy.NewConsentService(repo)
	tenantID := uuid.New()
	scope := tenantScope(tenantID)

	if err := svc.RecordConsent(context.Background(), scope, nil, "p", "anon-v1", "prompt-v1"); err != nil {
		t.Fatalf("RecordConsent: %v", err)
	}
	row, ok, _ := repo.Get(context.Background(), tenantID, aipolicy.ScopeTenant, tenantID.String())
	if !ok {
		t.Fatal("seed not found")
	}
	if row.ActorUserID != nil {
		t.Errorf("actor = %v; want nil", row.ActorUserID)
	}
}

func TestRecordConsent_BoundaryRejects(t *testing.T) {
	t.Parallel()
	repo := newFakeConsentRepo()
	svc, _ := aipolicy.NewConsentService(repo)
	good := tenantScope(uuid.New())

	cases := []struct {
		name    string
		scope   aipolicy.ConsentScope
		anonVer string
		prmtVer string
		want    error
	}{
		{"zero tenant", aipolicy.ConsentScope{Kind: aipolicy.ScopeTenant, ID: "x"}, "v1", "v1", aipolicy.ErrInvalidTenant},
		{"bad kind", aipolicy.ConsentScope{TenantID: uuid.New(), Kind: "bogus", ID: "x"}, "v1", "v1", aipolicy.ErrInvalidScopeType},
		{"blank id", aipolicy.ConsentScope{TenantID: uuid.New(), Kind: aipolicy.ScopeTenant, ID: ""}, "v1", "v1", aipolicy.ErrInvalidScopeID},
		{"blank anon", good, "", "v1", aipolicy.ErrInvalidAnonymizerVersion},
		{"blank prompt", good, "v1", " ", aipolicy.ErrInvalidPromptVersion},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := svc.RecordConsent(context.Background(), tc.scope, nil, "p", tc.anonVer, tc.prmtVer)
			if !errors.Is(err, tc.want) {
				t.Fatalf("RecordConsent(%q): err=%v; want %v", tc.name, err, tc.want)
			}
		})
	}
}

func TestRecordConsent_GetErrorBubbles(t *testing.T) {
	t.Parallel()
	repo := newFakeConsentRepo()
	repoErr := errors.New("transport boom")
	repo.getErr = repoErr
	svc, _ := aipolicy.NewConsentService(repo)

	err := svc.RecordConsent(context.Background(), tenantScope(uuid.New()), nil, "p", "v1", "v1")
	if !errors.Is(err, repoErr) {
		t.Fatalf("RecordConsent: %v; want wrapping repoErr", err)
	}
}

func TestRecordConsent_UpsertErrorBubbles(t *testing.T) {
	t.Parallel()
	repo := newFakeConsentRepo()
	repoErr := errors.New("upsert boom")
	repo.upsertErr = repoErr
	svc, _ := aipolicy.NewConsentService(repo)

	err := svc.RecordConsent(context.Background(), tenantScope(uuid.New()), nil, "p", "v1", "v1")
	if !errors.Is(err, repoErr) {
		t.Fatalf("RecordConsent: %v; want wrapping repoErr", err)
	}
}

func TestHasConsent_NonTenantScopesRoundTrip(t *testing.T) {
	t.Parallel()
	repo := newFakeConsentRepo()
	svc, _ := aipolicy.NewConsentService(repo)
	tenantID := uuid.New()
	channel := aipolicy.ConsentScope{TenantID: tenantID, Kind: aipolicy.ScopeChannel, ID: "whatsapp"}
	team := aipolicy.ConsentScope{TenantID: tenantID, Kind: aipolicy.ScopeTeam, ID: uuid.New().String()}

	for _, sc := range []aipolicy.ConsentScope{channel, team} {
		if err := svc.RecordConsent(context.Background(), sc, nil, "p", "anon-v1", "prompt-v1"); err != nil {
			t.Fatalf("RecordConsent(%v): %v", sc.Kind, err)
		}
		has, err := svc.HasConsent(context.Background(), sc, "anon-v1", "prompt-v1")
		if err != nil {
			t.Fatalf("HasConsent(%v): %v", sc.Kind, err)
		}
		if !has {
			t.Errorf("scope %v: HasConsent = false; want true", sc.Kind)
		}
	}
}
