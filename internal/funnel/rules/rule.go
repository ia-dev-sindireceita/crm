package rules

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// Scope is the cascade specificity bucket of a rule. The value is
// derived from the storage shape: the migration 0102 columns `channel`
// (text NULL) and `team_id` (uuid NULL) jointly encode the scope so
// the rule table needs no extra discriminator column.
type Scope string

const (
	// ScopeChannel — channel column non-empty. Most specific. Wins
	// over team and tenant on the same trigger.
	ScopeChannel Scope = "channel"

	// ScopeTeam — channel empty, team_id populated. Mid-specificity;
	// wins over tenant only.
	ScopeTeam Scope = "team"

	// ScopeTenant — both channel and team_id NULL. Tenant-wide
	// default; loses to channel and team on the same trigger.
	ScopeTenant Scope = "tenant"
)

// scopeRank maps the cascade order to an integer used for stable
// sort ordering (lower wins). Internal — callers compare Scope
// values directly.
func scopeRank(s Scope) int {
	switch s {
	case ScopeChannel:
		return 0
	case ScopeTeam:
		return 1
	case ScopeTenant:
		return 2
	default:
		return 99
	}
}

// TriggerType is the discriminator that tells the resolver how to
// extract a dedup signature from the opaque trigger_config jsonb.
// The set is closed for the resolver's purposes but the persisted
// column is a free text — unknown values are treated as
// non-deduplicating so new trigger kinds can ship before this enum
// learns them.
type TriggerType string

const (
	TriggerTypeMessageContains     TriggerType = "message_contains"
	TriggerTypeCampaignClick       TriggerType = "campaign_click"
	TriggerTypeMessageKeywordRegex TriggerType = "message_keyword_regex"
)

// ActionType mirrors TriggerType for the action side. Fase 4 only
// ships `move_to_stage`; future actions extend the enum without
// breaking the resolver, which never inspects the action body.
type ActionType string

const (
	ActionTypeMoveToStage ActionType = "move_to_stage"
)

// Rule is the per-tenant automation entity. The struct mirrors the
// funnel_rules table row 1:1 so the postgres adapter can scan into a
// Rule without an intermediate DTO.
//
// Channel and TeamID together encode the scope (see [Scope]); the
// two slots are mutually exclusive in practice — when both are
// populated the rule is interpreted as channel-scoped (channel wins,
// team_id is ignored). [NewRule] enforces the canonical shape.
type Rule struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	Channel       string     // empty when scope ≠ channel
	TeamID        *uuid.UUID // nil when scope ≠ team
	Name          string
	TriggerType   TriggerType
	TriggerConfig map[string]any // opaque jsonb body; per-type shape
	ActionType    ActionType
	ActionConfig  map[string]any // opaque jsonb body; per-type shape
	Enabled       bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Scope returns the cascade bucket the rule belongs to. The function
// is total: a fresh zero-value Rule reports ScopeTenant because
// neither channel nor team_id is set.
func (r Rule) Scope() Scope {
	if strings.TrimSpace(r.Channel) != "" {
		return ScopeChannel
	}
	if r.TeamID != nil && *r.TeamID != uuid.Nil {
		return ScopeTeam
	}
	return ScopeTenant
}

// TriggerSignature returns the per-rule dedup key used by the cascade
// resolver. Two rules with the same signature compete for the same
// "slot" in the resolved set; the most-specific scope wins.
//
// The signature is type-aware: each known TriggerType pulls its
// canonical field from TriggerConfig and normalises it (trim +
// lowercase, mirroring the SIN-62953 ADR D5 canonicalisation note).
// An unrecognised TriggerType returns an empty string, which the
// resolver treats as "do not deduplicate" — forward-compatible with
// trigger kinds the domain enum has not learned yet.
func (r Rule) TriggerSignature() string {
	switch r.TriggerType {
	case TriggerTypeMessageContains:
		if v, ok := stringField(r.TriggerConfig, "phrase"); ok {
			return string(TriggerTypeMessageContains) + ":phrase=" + canonicalise(v)
		}
	case TriggerTypeCampaignClick:
		if v, ok := stringField(r.TriggerConfig, "campaign_id"); ok {
			return string(TriggerTypeCampaignClick) + ":campaign_id=" + strings.TrimSpace(v)
		}
		if v, ok := stringField(r.TriggerConfig, "slug"); ok {
			return string(TriggerTypeCampaignClick) + ":slug=" + canonicalise(v)
		}
	case TriggerTypeMessageKeywordRegex:
		if v, ok := stringField(r.TriggerConfig, "regex"); ok {
			// regex is case-sensitive by author intent — only trim
			// surrounding whitespace; do NOT lower-case.
			return string(TriggerTypeMessageKeywordRegex) + ":regex=" + strings.TrimSpace(v)
		}
	}
	return ""
}

// canonicalise applies the lower-case + trim normalisation the ADR
// D5 pins for keyword triggers. NFC normalisation is deferred to a
// follow-up — UI input already lands in NFC on every modern browser
// and the test corpus reflects that.
func canonicalise(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// stringField pulls a string-typed value out of an untyped JSON map.
// Returns ok=false when the map is nil, the key is missing, or the
// value is not a string. Other JSON types (numbers, bools) are NOT
// coerced — they fail the type assertion, which keeps the per-type
// trigger contract narrow.
func stringField(m map[string]any, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	v, ok := m[key].(string)
	return v, ok
}

// NewRule constructs a Rule with the structural invariants enforced.
// Used by the HTMX editor (SIN-62961) when authoring or persisting a
// rule; the resolver does NOT call this on read because the postgres
// adapter trusts the row shape coming out of the database.
//
// Validation contract:
//
//   - tenantID MUST be non-nil — RLS would reject the insert anyway,
//     but failing early gives the form a typed error to render inline.
//   - name MUST be non-empty after Trim.
//   - triggerType MUST be one of the documented values; unknown
//     triggers are rejected at the form (the resolver tolerates them
//     for forward compatibility on rows authored by future versions).
//   - actionType MUST be one of the documented values.
//   - For known trigger types, the canonical config field MUST be a
//     non-empty string. Each TriggerType pulls a different key:
//     message_contains → "phrase", campaign_click → "campaign_id" OR
//     "slug", message_keyword_regex → "regex".
//   - For move_to_stage actions, action_config MUST carry a non-empty
//     "stage_key" string. The resolver doesn't inspect this, but the
//     downstream auto-handoff consumer would explode on an empty key.
//
// Scope columns (channel + teamID) are not validated here — the [Scope]
// method derives the bucket from whatever the caller supplied. The
// editor parses radio buttons into (channel, teamID) pairs; tests can
// pass either or both, with channel taking precedence at resolve time.
//
// id defaults to a fresh uuid when zero, mirroring NewCampaign. now
// stamps both CreatedAt and UpdatedAt; the storage adapter is expected
// to overwrite UpdatedAt on Update.
func NewRule(
	id uuid.UUID,
	tenantID uuid.UUID,
	channel string,
	teamID *uuid.UUID,
	name string,
	triggerType TriggerType,
	triggerConfig map[string]any,
	actionType ActionType,
	actionConfig map[string]any,
	enabled bool,
	now time.Time,
) (Rule, error) {
	if tenantID == uuid.Nil {
		return Rule{}, ErrInvalidTenant
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Rule{}, ErrInvalidRule
	}
	if !triggerType.Known() {
		return Rule{}, ErrUnknownTriggerType
	}
	if !actionType.Known() {
		return Rule{}, ErrUnknownActionType
	}
	if err := validateTriggerConfig(triggerType, triggerConfig); err != nil {
		return Rule{}, err
	}
	if err := validateActionConfig(actionType, actionConfig); err != nil {
		return Rule{}, err
	}
	if id == uuid.Nil {
		id = uuid.New()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	channel = strings.TrimSpace(channel)
	var team *uuid.UUID
	if channel == "" && teamID != nil && *teamID != uuid.Nil {
		t := *teamID
		team = &t
	}
	if triggerConfig == nil {
		triggerConfig = map[string]any{}
	}
	if actionConfig == nil {
		actionConfig = map[string]any{}
	}
	return Rule{
		ID:            id,
		TenantID:      tenantID,
		Channel:       channel,
		TeamID:        team,
		Name:          name,
		TriggerType:   triggerType,
		TriggerConfig: triggerConfig,
		ActionType:    actionType,
		ActionConfig:  actionConfig,
		Enabled:       enabled,
		CreatedAt:     now,
		UpdatedAt:     now,
	}, nil
}

// Known reports whether t is one of the documented TriggerType values.
// Used by [NewRule] to reject unknown values at the form boundary.
func (t TriggerType) Known() bool {
	switch t {
	case TriggerTypeMessageContains,
		TriggerTypeCampaignClick,
		TriggerTypeMessageKeywordRegex:
		return true
	}
	return false
}

// Known reports whether a is one of the documented ActionType values.
func (a ActionType) Known() bool {
	return a == ActionTypeMoveToStage
}

// validateTriggerConfig enforces the per-type required-field contract
// the form layer relies on. The set is intentionally narrow — only the
// fields TriggerSignature reads — so future trigger kinds can land
// without revising this function in lockstep.
func validateTriggerConfig(t TriggerType, cfg map[string]any) error {
	switch t {
	case TriggerTypeMessageContains:
		if v, ok := stringField(cfg, "phrase"); !ok || strings.TrimSpace(v) == "" {
			return ErrInvalidRule
		}
	case TriggerTypeCampaignClick:
		if v, ok := stringField(cfg, "campaign_id"); ok && strings.TrimSpace(v) != "" {
			return nil
		}
		if v, ok := stringField(cfg, "slug"); ok && strings.TrimSpace(v) != "" {
			return nil
		}
		return ErrInvalidRule
	case TriggerTypeMessageKeywordRegex:
		if v, ok := stringField(cfg, "regex"); !ok || strings.TrimSpace(v) == "" {
			return ErrInvalidRule
		}
	}
	return nil
}

// validateActionConfig enforces the action_config required-field
// contract.
func validateActionConfig(a ActionType, cfg map[string]any) error {
	if a == ActionTypeMoveToStage {
		if v, ok := stringField(cfg, "stage_key"); !ok || strings.TrimSpace(v) == "" {
			return ErrInvalidRule
		}
	}
	return nil
}
