package engine

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/funnel/rules"
)

// Tests live in the internal package so they can exercise the
// unexported matchTrigger / stageKey helpers without going through
// Engine.Handle.

func TestMatchTrigger_MessageContains_CaseInsensitive(t *testing.T) {
	t.Parallel()
	rule := rules.Rule{
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "ORÇAMENTO"},
	}
	msg := InboundMessage{Body: "quero um orçamento, por favor"}
	if !matchTrigger(rule, msg) {
		t.Error("expected match on case-insensitive substring")
	}
}

func TestMatchTrigger_MessageContains_NoMatch(t *testing.T) {
	t.Parallel()
	rule := rules.Rule{
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "orçamento"},
	}
	msg := InboundMessage{Body: "bom dia, tudo bem?"}
	if matchTrigger(rule, msg) {
		t.Error("expected no match")
	}
}

func TestMatchTrigger_MessageContains_BlankPhraseDoesNotMatch(t *testing.T) {
	t.Parallel()
	rule := rules.Rule{
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "   "},
	}
	if matchTrigger(rule, InboundMessage{Body: "anything"}) {
		t.Error("blank phrase should not match every message")
	}
}

func TestMatchTrigger_MessageContains_MissingField(t *testing.T) {
	t.Parallel()
	rule := rules.Rule{
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{},
	}
	if matchTrigger(rule, InboundMessage{Body: "x"}) {
		t.Error("missing phrase field should not match")
	}
}

func TestMatchTrigger_RegexMatch(t *testing.T) {
	t.Parallel()
	rule := rules.Rule{
		TriggerType:   rules.TriggerTypeMessageKeywordRegex,
		TriggerConfig: map[string]any{"regex": `quanto\s+custa`},
	}
	if !matchTrigger(rule, InboundMessage{Body: "quanto custa o plano X?"}) {
		t.Error("expected regex to match")
	}
	if matchTrigger(rule, InboundMessage{Body: "Quanto Custa"}) {
		t.Error("regex is case-sensitive — should not match upper-cased input")
	}
}

func TestMatchTrigger_RegexInvalidIsNonMatch(t *testing.T) {
	t.Parallel()
	rule := rules.Rule{
		TriggerType:   rules.TriggerTypeMessageKeywordRegex,
		TriggerConfig: map[string]any{"regex": `[unclosed`},
	}
	if matchTrigger(rule, InboundMessage{Body: "anything"}) {
		t.Error("invalid regex should not match anything")
	}
}

func TestMatchTrigger_RegexBlankIsNonMatch(t *testing.T) {
	t.Parallel()
	rule := rules.Rule{
		TriggerType:   rules.TriggerTypeMessageKeywordRegex,
		TriggerConfig: map[string]any{"regex": "   "},
	}
	if matchTrigger(rule, InboundMessage{Body: "anything"}) {
		t.Error("blank regex should not match")
	}
}

func TestMatchTrigger_CampaignClick_DeferredReturnsFalse(t *testing.T) {
	t.Parallel()
	rule := rules.Rule{
		TriggerType:   rules.TriggerTypeCampaignClick,
		TriggerConfig: map[string]any{"campaign_id": uuid.NewString()},
	}
	if matchTrigger(rule, InboundMessage{Body: "irrelevant"}) {
		t.Error("campaign_click is deferred — should always return false today")
	}
}

func TestMatchTrigger_UnknownTriggerType(t *testing.T) {
	t.Parallel()
	rule := rules.Rule{
		TriggerType:   rules.TriggerType("never_heard_of"),
		TriggerConfig: map[string]any{},
	}
	if matchTrigger(rule, InboundMessage{Body: "x"}) {
		t.Error("unknown trigger type should not match")
	}
}

func TestStageKey_PrefersStageKey(t *testing.T) {
	t.Parallel()
	rule := rules.Rule{
		ActionType:   rules.ActionTypeMoveToStage,
		ActionConfig: map[string]any{"stage_key": "qualificando", "stage": "novo"},
	}
	k, ok := stageKey(rule)
	if !ok || k != "qualificando" {
		t.Errorf("stageKey = %q, ok=%v, want qualificando, true", k, ok)
	}
}

func TestStageKey_LegacyStageAlias(t *testing.T) {
	t.Parallel()
	rule := rules.Rule{
		ActionType:   rules.ActionTypeMoveToStage,
		ActionConfig: map[string]any{"stage": "ganho"},
	}
	k, ok := stageKey(rule)
	if !ok || k != "ganho" {
		t.Errorf("stageKey (alias) = %q, ok=%v, want ganho, true", k, ok)
	}
}

func TestStageKey_BlankRejected(t *testing.T) {
	t.Parallel()
	rule := rules.Rule{
		ActionType:   rules.ActionTypeMoveToStage,
		ActionConfig: map[string]any{"stage_key": "   "},
	}
	if k, ok := stageKey(rule); ok {
		t.Errorf("stageKey on blank input = %q, ok=true; want skip", k)
	}
}

func TestStageKey_RequiresMoveToStage(t *testing.T) {
	t.Parallel()
	rule := rules.Rule{
		ActionType:   rules.ActionType("hypothetical_send_template"),
		ActionConfig: map[string]any{"stage_key": "qualificando"},
	}
	if _, ok := stageKey(rule); ok {
		t.Error("stageKey should only resolve for ActionTypeMoveToStage")
	}
}

func TestRegexCache_HotReuse(t *testing.T) {
	t.Parallel()
	// Two consecutive matches against the same expression must yield
	// the same compiled instance from the sync.Map cache.
	expr := `(?i)preço`
	r1, err := compileRegex(expr)
	if err != nil {
		t.Fatalf("compileRegex first call: %v", err)
	}
	r2, err := compileRegex(expr)
	if err != nil {
		t.Fatalf("compileRegex second call: %v", err)
	}
	if r1 != r2 {
		t.Error("compileRegex did not reuse cached compile")
	}
}

func TestInMemoryApplications_DedupAndSnapshot(t *testing.T) {
	t.Parallel()
	repo := NewInMemoryApplications()
	app := Application{
		TenantID:       uuid.New(),
		RuleID:         uuid.New(),
		MessageID:      uuid.New(),
		ConversationID: uuid.New(),
		ActionType:     "move_to_stage",
		AppliedAt:      time.Now().UTC(),
	}
	if err := repo.Record(context.Background(), app); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	if err := repo.Record(context.Background(), app); err != ErrAlreadyApplied {
		t.Fatalf("second Record = %v, want ErrAlreadyApplied", err)
	}
	if repo.Len() != 1 {
		t.Errorf("Len = %d, want 1", repo.Len())
	}
	snap := repo.Snapshot()
	if len(snap) != 1 || snap[0].MessageID != app.MessageID {
		t.Errorf("Snapshot = %+v", snap)
	}
}
