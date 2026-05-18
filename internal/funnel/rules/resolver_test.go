package rules

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// uuidFrom builds a deterministic uuid from a byte. Using a literal
// byte instead of uuid.New keeps test assertions on Rule.ID
// reproducible across runs without per-test setup churn.
func uuidFrom(b byte) uuid.UUID {
	var u uuid.UUID
	u[15] = b
	return u
}

// ptr returns a pointer to v. Helper for the *uuid.UUID slot on Rule.
func ptr[T any](v T) *T { return &v }

// at returns a deterministic timestamp offset by n minutes from a
// fixed epoch. Lets fixtures order rules predictably without manual
// time.Date plumbing.
func at(n int) time.Time {
	return time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Minute)
}

// rule is a fixture builder for the test corpus below. The
// non-overridden fields default to "tenant scope, enabled,
// move_to_stage". Tests override only the fields they assert on so
// the corpus stays terse.
func rule(id byte, tenant uuid.UUID, opts ...func(*Rule)) Rule {
	r := Rule{
		ID:            uuidFrom(id),
		TenantID:      tenant,
		Name:          "fixture",
		TriggerType:   TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "preço"},
		ActionType:    ActionTypeMoveToStage,
		ActionConfig:  map[string]any{"stage_key": "qualificando"},
		Enabled:       true,
		CreatedAt:     at(int(id)),
		UpdatedAt:     at(int(id)),
	}
	for _, opt := range opts {
		opt(&r)
	}
	return r
}

func withChannel(ch string) func(*Rule) { return func(r *Rule) { r.Channel = ch } }
func withTeam(id uuid.UUID) func(*Rule) { return func(r *Rule) { r.TeamID = ptr(id) } }
func withPhrase(s string) func(*Rule) {
	return func(r *Rule) { r.TriggerConfig = map[string]any{"phrase": s} }
}
func withDisabled() func(*Rule) { return func(r *Rule) { r.Enabled = false } }
func withAction(stage string) func(*Rule) {
	return func(r *Rule) { r.ActionConfig = map[string]any{"stage_key": stage} }
}
func withTrigger(t TriggerType, cfg map[string]any) func(*Rule) {
	return func(r *Rule) {
		r.TriggerType = t
		r.TriggerConfig = cfg
	}
}
func withCreatedAt(t time.Time) func(*Rule) {
	return func(r *Rule) {
		r.CreatedAt = t
		r.UpdatedAt = t
	}
}

// ---------------------------------------------------------------------------
// Scope() derivation
// ---------------------------------------------------------------------------

func TestRule_Scope_DerivedFromColumns(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	team := uuid.New()

	cases := []struct {
		name string
		rule Rule
		want Scope
	}{
		{"channel populated", rule(1, tenant, withChannel("webchat")), ScopeChannel},
		{"channel whitespace only collapses to tenant", rule(2, tenant, withChannel("   ")), ScopeTenant},
		{"team set, no channel", rule(3, tenant, withTeam(team)), ScopeTeam},
		{"channel beats team when both set", rule(4, tenant, withChannel("webchat"), withTeam(team)), ScopeChannel},
		{"both nil collapses to tenant", rule(5, tenant), ScopeTenant},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.rule.Scope(); got != tc.want {
				t.Fatalf("Scope() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TriggerSignature canonicalisation + per-type contracts
// ---------------------------------------------------------------------------

func TestRule_TriggerSignature(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()

	cases := []struct {
		name string
		rule Rule
		want string
	}{
		{
			"message_contains lower-cases and trims phrase",
			rule(1, tenant, withTrigger(TriggerTypeMessageContains, map[string]any{"phrase": "  Orçamento  "})),
			"message_contains:phrase=orçamento",
		},
		{
			"campaign_click prefers campaign_id over slug",
			rule(2, tenant, withTrigger(TriggerTypeCampaignClick, map[string]any{"campaign_id": "camp-x", "slug": "ignored"})),
			"campaign_click:campaign_id=camp-x",
		},
		{
			"campaign_click falls back to slug normalised when campaign_id absent",
			rule(3, tenant, withTrigger(TriggerTypeCampaignClick, map[string]any{"slug": "  Black-Friday-2026  "})),
			"campaign_click:slug=black-friday-2026",
		},
		{
			"message_keyword_regex keeps case (regex is case-sensitive)",
			rule(4, tenant, withTrigger(TriggerTypeMessageKeywordRegex, map[string]any{"regex": "(?i)NF-\\d+"})),
			"message_keyword_regex:regex=(?i)NF-\\d+",
		},
		{
			"unknown trigger type → empty signature",
			rule(5, tenant, withTrigger("conversation_idle", map[string]any{"after": "PT1H"})),
			"",
		},
		{
			"known trigger type with missing field → empty signature",
			rule(6, tenant, withTrigger(TriggerTypeMessageContains, map[string]any{"other": "data"})),
			"",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.rule.TriggerSignature(); got != tc.want {
				t.Fatalf("TriggerSignature() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Resolver — AC#3 worked example (the SIN-62197 acceptance scenario)
// ---------------------------------------------------------------------------

// TestResolver_AC3_WebchatRuleIsolatedFromWhatsApp covers AC#2 of
// SIN-62955: "regra 'msg contém orçamento → qualificando' no escopo
// canal Webchat ativa quando msg do canal Webchat; ignora msg do
// WhatsApp do mesmo tenant".
func TestResolver_AC3_WebchatRuleIsolatedFromWhatsApp(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	repo := NewInMemoryRepository()
	repo.Seed(
		rule(1, tenant,
			withChannel("webchat"),
			withPhrase("orçamento"),
			withAction("qualificando"),
		),
	)
	resolver, err := NewResolver(repo)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	// Webchat event — the channel-scoped rule must fire.
	got, err := resolver.Resolve(context.Background(), ResolveInput{
		TenantID: tenant,
		Channel:  "webchat",
	})
	if err != nil {
		t.Fatalf("Resolve(webchat): %v", err)
	}
	if len(got) != 1 || got[0].SourceScope != ScopeChannel || got[0].Rule.ID != uuidFrom(1) {
		t.Fatalf("webchat: want 1 channel rule id=%v, got %+v", uuidFrom(1), got)
	}

	// WhatsApp event — same tenant, no rule should fire.
	got, err = resolver.Resolve(context.Background(), ResolveInput{
		TenantID: tenant,
		Channel:  "whatsapp",
	})
	if err != nil {
		t.Fatalf("Resolve(whatsapp): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("whatsapp: want 0 rules, got %d (%+v)", len(got), got)
	}
}

// ---------------------------------------------------------------------------
// Resolver — cascade across all three scopes
// ---------------------------------------------------------------------------

func TestResolver_Cascade_ChannelBeatsTeamBeatsTenantOnSameTrigger(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	team := uuid.New()

	repo := NewInMemoryRepository()
	repo.Seed(
		rule(1, tenant, withAction("tenant-target")),                                 // tenant scope, phrase=preço
		rule(2, tenant, withTeam(team), withAction("team-target")),                   // team scope, phrase=preço
		rule(3, tenant, withChannel("webchat"), withAction("channel-target")),        // channel scope, phrase=preço
		rule(4, tenant, withPhrase("compra"), withAction("distinct-trigger-tenant")), // distinct trigger, tenant scope
	)
	resolver, _ := NewResolver(repo)

	got, err := resolver.Resolve(context.Background(), ResolveInput{
		TenantID: tenant,
		Channel:  "webchat",
		TeamID:   team,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rules (channel wins for preço + tenant distinct trigger), got %d (%+v)", len(got), got)
	}
	// Cascade order: channel rules first, then tenant.
	if got[0].SourceScope != ScopeChannel || got[0].Rule.ActionConfig["stage_key"] != "channel-target" {
		t.Fatalf("rule[0]: want channel/channel-target, got %+v", got[0])
	}
	if got[1].SourceScope != ScopeTenant || got[1].Rule.ActionConfig["stage_key"] != "distinct-trigger-tenant" {
		t.Fatalf("rule[1]: want tenant/distinct-trigger-tenant, got %+v", got[1])
	}
}

func TestResolver_Cascade_TeamWinsWhenNoChannelRule(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	team := uuid.New()

	repo := NewInMemoryRepository()
	repo.Seed(
		rule(1, tenant, withAction("tenant-target")),                           // tenant scope
		rule(2, tenant, withTeam(team), withAction("team-target")),             // team scope
		rule(3, tenant, withChannel("instagram"), withAction("wrong-channel")), // channel scope: wrong channel
	)
	resolver, _ := NewResolver(repo)

	got, err := resolver.Resolve(context.Background(), ResolveInput{
		TenantID: tenant,
		Channel:  "webchat", // no webchat-scoped rule
		TeamID:   team,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || got[0].SourceScope != ScopeTeam || got[0].Rule.ActionConfig["stage_key"] != "team-target" {
		t.Fatalf("want 1 team rule with team-target, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Resolver — enabled flag is honoured
// ---------------------------------------------------------------------------

func TestResolver_DisabledRulesAreIgnored(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	repo := NewInMemoryRepository()
	repo.Seed(
		rule(1, tenant, withChannel("webchat"), withDisabled(), withAction("disabled-channel")),
		rule(2, tenant, withAction("tenant-target")), // tenant fallback, enabled
	)
	resolver, _ := NewResolver(repo)

	got, err := resolver.Resolve(context.Background(), ResolveInput{
		TenantID: tenant,
		Channel:  "webchat",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// The disabled channel rule is silently ignored — both at the repo
	// level (enabled=false filter) and at the cascade level (cannot
	// shadow). The tenant rule wins.
	if len(got) != 1 || got[0].SourceScope != ScopeTenant || got[0].Rule.ActionConfig["stage_key"] != "tenant-target" {
		t.Fatalf("want tenant rule (disabled channel ignored), got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Resolver — cross-tenant isolation
// ---------------------------------------------------------------------------

func TestResolver_CrossTenantIsolation(t *testing.T) {
	t.Parallel()
	tenantA := uuid.New()
	tenantB := uuid.New()
	repo := NewInMemoryRepository()
	repo.Seed(
		rule(1, tenantA, withChannel("webchat"), withAction("a-channel")),
		rule(2, tenantB, withChannel("webchat"), withAction("b-channel")),
	)
	resolver, _ := NewResolver(repo)

	got, err := resolver.Resolve(context.Background(), ResolveInput{
		TenantID: tenantA,
		Channel:  "webchat",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || got[0].Rule.TenantID != tenantA || got[0].Rule.ActionConfig["stage_key"] != "a-channel" {
		t.Fatalf("want tenantA rule only, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Resolver — deterministic ordering across runs
// ---------------------------------------------------------------------------

func TestResolver_DeterministicOrdering(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	repo := NewInMemoryRepository()
	// Seed two distinct-trigger tenant rules with identical
	// CreatedAt — ordering must break by id lexicographically.
	t1 := at(10)
	repo.Seed(
		rule(2, tenant,
			withPhrase("compra"),
			withAction("compra-action"),
			withCreatedAt(t1),
		),
		rule(1, tenant,
			withPhrase("preço"),
			withAction("preco-action"),
			withCreatedAt(t1),
		),
	)
	resolver, _ := NewResolver(repo)

	want := []string{"preco-action", "compra-action"} // id=1 sorts before id=2

	// Run 10 times — every iteration must produce the same order.
	for i := 0; i < 10; i++ {
		got, err := resolver.Resolve(context.Background(), ResolveInput{TenantID: tenant})
		if err != nil {
			t.Fatalf("Resolve[%d]: %v", i, err)
		}
		if len(got) != 2 {
			t.Fatalf("Resolve[%d]: want 2 rules, got %d", i, len(got))
		}
		for k, w := range want {
			if got[k].Rule.ActionConfig["stage_key"] != w {
				t.Fatalf("Resolve[%d] rule[%d]: want %q, got %q (full=%+v)",
					i, k, w, got[k].Rule.ActionConfig["stage_key"], got)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Resolver — input validation + repository error propagation
// ---------------------------------------------------------------------------

func TestResolver_RejectsNilTenantID(t *testing.T) {
	t.Parallel()
	repo := NewInMemoryRepository()
	resolver, _ := NewResolver(repo)
	_, err := resolver.Resolve(context.Background(), ResolveInput{})
	if err != ErrInvalidTenant {
		t.Fatalf("want ErrInvalidTenant, got %v", err)
	}
}

func TestResolver_RejectsNilRepository(t *testing.T) {
	t.Parallel()
	if _, err := NewResolver(nil); err == nil {
		t.Fatal("want error for nil repo, got nil")
	}
}

// ---------------------------------------------------------------------------
// Cascade with rules that have no signature (unknown trigger types
// or malformed config) — these MUST always survive because they
// cannot be deduplicated.
// ---------------------------------------------------------------------------

func TestResolver_UnsignedRulesAllAdopted(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	repo := NewInMemoryRepository()
	repo.Seed(
		rule(1, tenant,
			withChannel("webchat"),
			withTrigger("future_kind_v2", map[string]any{"x": "y"}),
		),
		rule(2, tenant,
			withTrigger("future_kind_v2", map[string]any{"x": "z"}),
		),
	)
	resolver, _ := NewResolver(repo)

	got, err := resolver.Resolve(context.Background(), ResolveInput{TenantID: tenant, Channel: "webchat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want both unsigned rules adopted (no dedup), got %d (%+v)", len(got), got)
	}
}
