package aipolicy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aipolicy"
)

// fakeRepo is an in-memory aipolicy.Repository keyed by
// (tenant, scope_type, scope_id). The resolver tests inject one per
// case and assert which lookups were attempted via the calls slice.
type fakeRepo struct {
	rows  map[fakeKey]aipolicy.Policy
	calls []fakeKey
	// failOn, when set, returns this error from Get on the matching
	// lookup. Used to assert error wrapping per cascade level.
	failOn   fakeKey
	failWith error
}

type fakeKey struct {
	tenant    uuid.UUID
	scopeType aipolicy.ScopeType
	scopeID   string
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{rows: map[fakeKey]aipolicy.Policy{}}
}

func (f *fakeRepo) put(p aipolicy.Policy) {
	f.rows[fakeKey{tenant: p.TenantID, scopeType: p.ScopeType, scopeID: p.ScopeID}] = p
}

func (f *fakeRepo) Get(_ context.Context, tenantID uuid.UUID, scopeType aipolicy.ScopeType, scopeID string) (aipolicy.Policy, bool, error) {
	k := fakeKey{tenant: tenantID, scopeType: scopeType, scopeID: scopeID}
	f.calls = append(f.calls, k)
	if f.failWith != nil && k == f.failOn {
		return aipolicy.Policy{}, false, f.failWith
	}
	p, ok := f.rows[k]
	return p, ok, nil
}

func (f *fakeRepo) Upsert(_ context.Context, _ aipolicy.Policy) error {
	return errors.New("fakeRepo: Upsert not used in resolver tests")
}

func (f *fakeRepo) List(_ context.Context, _ uuid.UUID) ([]aipolicy.Policy, error) {
	return nil, errors.New("fakeRepo: List not used in resolver tests")
}

func (f *fakeRepo) Delete(_ context.Context, _ uuid.UUID, _ aipolicy.ScopeType, _ string) (bool, error) {
	return false, errors.New("fakeRepo: Delete not used in resolver tests")
}

func mkPolicy(t *testing.T, tenant uuid.UUID, scope aipolicy.ScopeType, scopeID, model string) aipolicy.Policy {
	t.Helper()
	return aipolicy.Policy{
		TenantID:      tenant,
		ScopeType:     scope,
		ScopeID:       scopeID,
		Model:         model,
		PromptVersion: "v1",
		Tone:          "neutro",
		Language:      "pt-BR",
		AIEnabled:     true,
		Anonymize:     true,
		OptIn:         true,
	}
}

func strPtr(s string) *string { return &s }

// The eight combinatorial cases the issue's acceptance criterion #2
// names: channel present/absent, team present/absent, and tenant row
// present/absent — minus the redundant (channel hits / tenant absent)
// pair (channel always wins on hit regardless of tenant). The five
// most-specific-wins shapes plus the three fallback shapes total
// eight cases.
func TestResolver_Cascade(t *testing.T) {
	t.Parallel()

	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	channelKey := "whatsapp"
	teamKey := "22222222-2222-2222-2222-222222222222"

	channelPolicy := mkPolicy(t, tenant, aipolicy.ScopeChannel, channelKey, "openrouter/channel-model")
	teamPolicy := mkPolicy(t, tenant, aipolicy.ScopeTeam, teamKey, "openrouter/team-model")
	tenantPolicy := mkPolicy(t, tenant, aipolicy.ScopeTenant, tenant.String(), "openrouter/tenant-model")

	tests := []struct {
		name       string
		seed       []aipolicy.Policy
		in         aipolicy.ResolveInput
		wantSource aipolicy.ResolveSource
		wantModel  string
		// wantCalls is the ordered sequence of lookups the resolver
		// must run for this case. The cascade short-circuits on hit,
		// so a channel hit must NOT trigger a tenant lookup.
		wantCalls []fakeKey
	}{
		{
			name:       "channel+team+tenant all set, channel row present → channel wins",
			seed:       []aipolicy.Policy{channelPolicy, teamPolicy, tenantPolicy},
			in:         aipolicy.ResolveInput{TenantID: tenant, ChannelID: strPtr(channelKey), TeamID: strPtr(teamKey)},
			wantSource: aipolicy.SourceChannel,
			wantModel:  "openrouter/channel-model",
			wantCalls: []fakeKey{
				{tenant, aipolicy.ScopeChannel, channelKey},
			},
		},
		{
			name:       "channel+team set, channel miss but team row present → team wins",
			seed:       []aipolicy.Policy{teamPolicy, tenantPolicy},
			in:         aipolicy.ResolveInput{TenantID: tenant, ChannelID: strPtr(channelKey), TeamID: strPtr(teamKey)},
			wantSource: aipolicy.SourceTeam,
			wantModel:  "openrouter/team-model",
			wantCalls: []fakeKey{
				{tenant, aipolicy.ScopeChannel, channelKey},
				{tenant, aipolicy.ScopeTeam, teamKey},
			},
		},
		{
			name:       "channel+team set, both miss, tenant row present → tenant wins",
			seed:       []aipolicy.Policy{tenantPolicy},
			in:         aipolicy.ResolveInput{TenantID: tenant, ChannelID: strPtr(channelKey), TeamID: strPtr(teamKey)},
			wantSource: aipolicy.SourceTenant,
			wantModel:  "openrouter/tenant-model",
			wantCalls: []fakeKey{
				{tenant, aipolicy.ScopeChannel, channelKey},
				{tenant, aipolicy.ScopeTeam, teamKey},
				{tenant, aipolicy.ScopeTenant, tenant.String()},
			},
		},
		{
			name:       "channel+team set, every row absent → default fallback",
			seed:       nil,
			in:         aipolicy.ResolveInput{TenantID: tenant, ChannelID: strPtr(channelKey), TeamID: strPtr(teamKey)},
			wantSource: aipolicy.SourceDefault,
			wantModel:  "openrouter/auto",
			wantCalls: []fakeKey{
				{tenant, aipolicy.ScopeChannel, channelKey},
				{tenant, aipolicy.ScopeTeam, teamKey},
				{tenant, aipolicy.ScopeTenant, tenant.String()},
			},
		},
		{
			name:       "team only (channel nil), team row present → team wins, channel skipped",
			seed:       []aipolicy.Policy{teamPolicy, tenantPolicy},
			in:         aipolicy.ResolveInput{TenantID: tenant, TeamID: strPtr(teamKey)},
			wantSource: aipolicy.SourceTeam,
			wantModel:  "openrouter/team-model",
			wantCalls: []fakeKey{
				{tenant, aipolicy.ScopeTeam, teamKey},
			},
		},
		{
			name:       "team only (channel nil), team row absent → tenant wins",
			seed:       []aipolicy.Policy{tenantPolicy},
			in:         aipolicy.ResolveInput{TenantID: tenant, TeamID: strPtr(teamKey)},
			wantSource: aipolicy.SourceTenant,
			wantModel:  "openrouter/tenant-model",
			wantCalls: []fakeKey{
				{tenant, aipolicy.ScopeTeam, teamKey},
				{tenant, aipolicy.ScopeTenant, tenant.String()},
			},
		},
		{
			name:       "channel only (team nil), channel row present → channel wins, team skipped",
			seed:       []aipolicy.Policy{channelPolicy, tenantPolicy},
			in:         aipolicy.ResolveInput{TenantID: tenant, ChannelID: strPtr(channelKey)},
			wantSource: aipolicy.SourceChannel,
			wantModel:  "openrouter/channel-model",
			wantCalls: []fakeKey{
				{tenant, aipolicy.ScopeChannel, channelKey},
			},
		},
		{
			name:       "tenant only (no channel, no team), tenant row absent → default fallback",
			seed:       nil,
			in:         aipolicy.ResolveInput{TenantID: tenant},
			wantSource: aipolicy.SourceDefault,
			wantModel:  "openrouter/auto",
			wantCalls: []fakeKey{
				{tenant, aipolicy.ScopeTenant, tenant.String()},
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			for _, p := range tc.seed {
				repo.put(p)
			}
			resolver, err := aipolicy.NewResolver(repo)
			if err != nil {
				t.Fatalf("NewResolver: %v", err)
			}

			got, src, err := resolver.Resolve(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if src != tc.wantSource {
				t.Errorf("source = %q, want %q", src, tc.wantSource)
			}
			if got.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", got.Model, tc.wantModel)
			}
			if len(repo.calls) != len(tc.wantCalls) {
				t.Fatalf("calls = %v, want %v", repo.calls, tc.wantCalls)
			}
			for i := range tc.wantCalls {
				if repo.calls[i] != tc.wantCalls[i] {
					t.Errorf("calls[%d] = %+v, want %+v", i, repo.calls[i], tc.wantCalls[i])
				}
			}
		})
	}
}

// Empty-string ChannelID / TeamID must behave the same as nil — the
// cascade skips the level instead of looking up scope_id = "" (which
// could never match anything anyway and wastes a round-trip).
func TestResolver_EmptyScopeStringsBehaveAsNil(t *testing.T) {
	t.Parallel()
	tenant := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	tenantPolicy := mkPolicy(t, tenant, aipolicy.ScopeTenant, tenant.String(), "openrouter/tenant-model")

	repo := newFakeRepo()
	repo.put(tenantPolicy)
	resolver, err := aipolicy.NewResolver(repo)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	got, src, err := resolver.Resolve(context.Background(), aipolicy.ResolveInput{
		TenantID:  tenant,
		ChannelID: strPtr(""),
		TeamID:    strPtr(""),
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if src != aipolicy.SourceTenant {
		t.Errorf("source = %q, want %q", src, aipolicy.SourceTenant)
	}
	if got.Model != "openrouter/tenant-model" {
		t.Errorf("Model = %q, want tenant-model", got.Model)
	}
	if len(repo.calls) != 1 {
		t.Errorf("calls = %d, want exactly one (tenant lookup)", len(repo.calls))
	}
}

// DefaultPolicy must satisfy the deny-by-default posture from
// ADR-0041: AIEnabled=false, Anonymize=true, and the migration column
// defaults.
func TestDefaultPolicy_IsDenyByDefault(t *testing.T) {
	t.Parallel()
	p := aipolicy.DefaultPolicy()
	if p.AIEnabled {
		t.Errorf("DefaultPolicy.AIEnabled = true, want false (LGPD opt-in)")
	}
	if !p.Anonymize {
		t.Errorf("DefaultPolicy.Anonymize = false, want true (defense in depth)")
	}
	if p.OptIn {
		t.Errorf("DefaultPolicy.OptIn = true, want false (LGPD opt-in)")
	}
	if p.Model != "openrouter/auto" {
		t.Errorf("DefaultPolicy.Model = %q, want %q", p.Model, "openrouter/auto")
	}
	if p.PromptVersion != "v1" {
		t.Errorf("DefaultPolicy.PromptVersion = %q, want %q", p.PromptVersion, "v1")
	}
	if p.Tone != "neutro" {
		t.Errorf("DefaultPolicy.Tone = %q, want %q", p.Tone, "neutro")
	}
	if p.Language != "pt-BR" {
		t.Errorf("DefaultPolicy.Language = %q, want %q", p.Language, "pt-BR")
	}
}

func TestResolver_NilRepoRejected(t *testing.T) {
	t.Parallel()
	if _, err := aipolicy.NewResolver(nil); err == nil {
		t.Error("NewResolver(nil) err = nil, want non-nil")
	}
}

func TestResolver_RejectsZeroTenant(t *testing.T) {
	t.Parallel()
	resolver, err := aipolicy.NewResolver(newFakeRepo())
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if _, _, err := resolver.Resolve(context.Background(), aipolicy.ResolveInput{}); !errors.Is(err, aipolicy.ErrInvalidTenant) {
		t.Errorf("Resolve(zero tenant) err = %v, want ErrInvalidTenant", err)
	}
}

func TestResolver_PropagatesRepositoryErrors(t *testing.T) {
	t.Parallel()
	tenant := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	channelKey := "whatsapp"
	teamKey := "55555555-5555-5555-5555-555555555555"

	cases := []struct {
		name string
		key  fakeKey
		in   aipolicy.ResolveInput
	}{
		{
			name: "channel lookup fails",
			key:  fakeKey{tenant, aipolicy.ScopeChannel, channelKey},
			in:   aipolicy.ResolveInput{TenantID: tenant, ChannelID: strPtr(channelKey)},
		},
		{
			name: "team lookup fails",
			key:  fakeKey{tenant, aipolicy.ScopeTeam, teamKey},
			in:   aipolicy.ResolveInput{TenantID: tenant, TeamID: strPtr(teamKey)},
		},
		{
			name: "tenant lookup fails",
			key:  fakeKey{tenant, aipolicy.ScopeTenant, tenant.String()},
			in:   aipolicy.ResolveInput{TenantID: tenant},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			sentinel := errors.New("driver: connection reset")
			repo.failOn = tc.key
			repo.failWith = sentinel
			resolver, err := aipolicy.NewResolver(repo)
			if err != nil {
				t.Fatalf("NewResolver: %v", err)
			}
			_, _, err = resolver.Resolve(context.Background(), tc.in)
			if !errors.Is(err, sentinel) {
				t.Errorf("err = %v, want errors.Is(sentinel)", err)
			}
		})
	}
}

func TestScopeType_IsValid(t *testing.T) {
	t.Parallel()
	for _, s := range []aipolicy.ScopeType{aipolicy.ScopeTenant, aipolicy.ScopeTeam, aipolicy.ScopeChannel} {
		if !s.IsValid() {
			t.Errorf("IsValid(%q) = false, want true", s)
		}
	}
	for _, s := range []aipolicy.ScopeType{"", "global", "wildcard"} {
		if aipolicy.ScopeType(s).IsValid() {
			t.Errorf("IsValid(%q) = true, want false", s)
		}
	}
}
