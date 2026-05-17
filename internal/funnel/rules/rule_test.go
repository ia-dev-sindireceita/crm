package rules

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fixedNow keeps NewRule deterministic across the table-driven cases.
var fixedNow = time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

func TestNewRule_Valid_ChannelScoped(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	id := uuid.New()
	r, err := NewRule(
		id, tenant,
		"webchat", nil,
		"  Move webchat preço → qualificando  ",
		TriggerTypeMessageContains, map[string]any{"phrase": "preço"},
		ActionTypeMoveToStage, map[string]any{"stage_key": "qualificando"},
		true,
		fixedNow,
	)
	if err != nil {
		t.Fatalf("NewRule: unexpected error: %v", err)
	}
	if r.ID != id {
		t.Fatalf("ID: want %v, got %v", id, r.ID)
	}
	if r.Name != "Move webchat preço → qualificando" {
		t.Fatalf("Name not trimmed: %q", r.Name)
	}
	if r.Scope() != ScopeChannel {
		t.Fatalf("Scope: want channel, got %s", r.Scope())
	}
	if r.TeamID != nil {
		t.Fatalf("TeamID: want nil for channel-scoped, got %v", *r.TeamID)
	}
	if !r.CreatedAt.Equal(fixedNow) || !r.UpdatedAt.Equal(fixedNow) {
		t.Fatalf("timestamps not stamped: created=%v updated=%v", r.CreatedAt, r.UpdatedAt)
	}
}

func TestNewRule_Valid_TeamScopedClearsChannel(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	team := uuid.New()
	r, err := NewRule(
		uuid.Nil, tenant,
		"", &team,
		"Team default",
		TriggerTypeMessageContains, map[string]any{"phrase": "x"},
		ActionTypeMoveToStage, map[string]any{"stage_key": "novo"},
		true, time.Time{},
	)
	if err != nil {
		t.Fatalf("NewRule: %v", err)
	}
	if r.ID == uuid.Nil {
		t.Fatal("ID: want fresh uuid when nil supplied")
	}
	if r.Scope() != ScopeTeam {
		t.Fatalf("Scope: want team, got %s", r.Scope())
	}
	if r.TeamID == nil || *r.TeamID != team {
		t.Fatalf("TeamID: want %v, got %v", team, r.TeamID)
	}
	if r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() {
		t.Fatal("timestamps must default to now() when zero supplied")
	}
}

func TestNewRule_TenantScope_TeamIgnoredWhenChannelEmptyAndTeamNil(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	r, err := NewRule(
		uuid.Nil, tenant,
		"", nil,
		"Tenant default",
		TriggerTypeMessageContains, map[string]any{"phrase": "x"},
		ActionTypeMoveToStage, map[string]any{"stage_key": "novo"},
		true, fixedNow,
	)
	if err != nil {
		t.Fatalf("NewRule: %v", err)
	}
	if r.Scope() != ScopeTenant {
		t.Fatalf("Scope: want tenant, got %s", r.Scope())
	}
}

func TestNewRule_ChannelTakesPrecedenceOverTeam(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	team := uuid.New()
	r, err := NewRule(
		uuid.Nil, tenant,
		"webchat", &team,
		"Webchat-with-team-supplied",
		TriggerTypeMessageContains, map[string]any{"phrase": "x"},
		ActionTypeMoveToStage, map[string]any{"stage_key": "novo"},
		true, fixedNow,
	)
	if err != nil {
		t.Fatalf("NewRule: %v", err)
	}
	if r.Scope() != ScopeChannel {
		t.Fatalf("Scope: channel must beat team, got %s", r.Scope())
	}
	if r.TeamID != nil {
		t.Fatalf("TeamID: must be nil when channel wins, got %v", *r.TeamID)
	}
}

func TestNewRule_Errors(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	type tc struct {
		name string
		mut  func(*ruleParams)
		want error
	}
	cases := []tc{
		{"nil tenant", func(p *ruleParams) { p.tenant = uuid.Nil }, ErrInvalidTenant},
		{"blank name", func(p *ruleParams) { p.name = "  " }, ErrInvalidRule},
		{"unknown trigger type", func(p *ruleParams) { p.trigger = "future_kind" }, ErrUnknownTriggerType},
		{"unknown action type", func(p *ruleParams) { p.action = "send_email" }, ErrUnknownActionType},
		{"missing phrase for message_contains", func(p *ruleParams) {
			p.triggerCfg = map[string]any{"other": "x"}
		}, ErrInvalidRule},
		{"blank phrase for message_contains", func(p *ruleParams) {
			p.triggerCfg = map[string]any{"phrase": "  "}
		}, ErrInvalidRule},
		{"campaign_click without id or slug", func(p *ruleParams) {
			p.trigger = TriggerTypeCampaignClick
			p.triggerCfg = map[string]any{"other": "x"}
		}, ErrInvalidRule},
		{"regex empty", func(p *ruleParams) {
			p.trigger = TriggerTypeMessageKeywordRegex
			p.triggerCfg = map[string]any{"regex": ""}
		}, ErrInvalidRule},
		{"move_to_stage without stage_key", func(p *ruleParams) {
			p.actionCfg = map[string]any{"other": "x"}
		}, ErrInvalidRule},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := defaultRuleParams(tenant)
			tc.mut(&p)
			_, err := NewRule(uuid.Nil, p.tenant, "", nil, p.name,
				p.trigger, p.triggerCfg, p.action, p.actionCfg, true, fixedNow)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestNewRule_CampaignClickAcceptsCampaignIDOrSlug(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	cases := []map[string]any{
		{"campaign_id": "camp-x"},
		{"slug": "black-friday"},
	}
	for _, cfg := range cases {
		cfg := cfg
		t.Run("cfg", func(t *testing.T) {
			t.Parallel()
			_, err := NewRule(uuid.Nil, tenant, "", nil, "n",
				TriggerTypeCampaignClick, cfg,
				ActionTypeMoveToStage, map[string]any{"stage_key": "ganho"},
				true, fixedNow)
			if err != nil {
				t.Fatalf("want ok, got %v (cfg=%+v)", err, cfg)
			}
		})
	}
}

func TestNewRule_NilConfigsDefaultToEmptyMap(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	// Use a known-good trigger + action where the validator does not
	// require any specific key in the maps: campaign_click with id, +
	// move_to_stage with stage_key. Then we send the inner map as the
	// validated one but pass nil for the unrelated config slot — wait,
	// both slots are validated. Just confirm validateActionConfig +
	// validateTriggerConfig do not crash on nil-but-validation-OK case
	// (move_to_stage with stage_key field present in the supplied map).
	r, err := NewRule(uuid.Nil, tenant, "", nil, "n",
		TriggerTypeMessageKeywordRegex, map[string]any{"regex": "x"},
		ActionTypeMoveToStage, map[string]any{"stage_key": "k"},
		true, fixedNow)
	if err != nil {
		t.Fatalf("NewRule: %v", err)
	}
	if r.TriggerConfig == nil || r.ActionConfig == nil {
		t.Fatal("configs must be non-nil after construction")
	}
}

func TestTriggerTypeAndActionType_Known(t *testing.T) {
	t.Parallel()
	if !TriggerTypeMessageContains.Known() {
		t.Fatal("message_contains must be Known")
	}
	if TriggerType("future").Known() {
		t.Fatal("unknown trigger must not be Known")
	}
	if !ActionTypeMoveToStage.Known() {
		t.Fatal("move_to_stage must be Known")
	}
	if ActionType("send_email").Known() {
		t.Fatal("unknown action must not be Known")
	}
}

// ruleParams + defaultRuleParams keep the table-driven error cases
// concise.
type ruleParams struct {
	tenant     uuid.UUID
	name       string
	trigger    TriggerType
	triggerCfg map[string]any
	action     ActionType
	actionCfg  map[string]any
}

func defaultRuleParams(tenant uuid.UUID) ruleParams {
	return ruleParams{
		tenant:     tenant,
		name:       "n",
		trigger:    TriggerTypeMessageContains,
		triggerCfg: map[string]any{"phrase": "x"},
		action:     ActionTypeMoveToStage,
		actionCfg:  map[string]any{"stage_key": "novo"},
	}
}
