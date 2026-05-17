package engine_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/funnel/engine"
	"github.com/pericles-luz/crm/internal/funnel/rules"
)

// recordingMover is a fake [engine.StageMover] used across the
// table-driven cases. It records every call and lets a test inject a
// boobytrap error for the next call.
type recordingMover struct {
	mu       sync.Mutex
	calls    []moveCall
	nextErr  error
	failOnce bool
}

type moveCall struct {
	tenantID       uuid.UUID
	conversationID uuid.UUID
	stageKey       string
	actor          uuid.UUID
	reason         string
}

func (m *recordingMover) MoveConversation(_ context.Context, tenantID, conversationID uuid.UUID, stage string, actor uuid.UUID, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, moveCall{
		tenantID:       tenantID,
		conversationID: conversationID,
		stageKey:       stage,
		actor:          actor,
		reason:         reason,
	})
	if m.nextErr != nil {
		err := m.nextErr
		if m.failOnce {
			m.nextErr = nil
		}
		return err
	}
	return nil
}

func (m *recordingMover) snapshot() []moveCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]moveCall, len(m.calls))
	copy(out, m.calls)
	return out
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newResolver(t *testing.T, repo *rules.InMemoryRepository) *rules.Resolver {
	t.Helper()
	r, err := rules.NewResolver(repo)
	if err != nil {
		t.Fatalf("rules.NewResolver: %v", err)
	}
	return r
}

func newEngine(t *testing.T, repo *rules.InMemoryRepository, apps engine.ApplicationsRepo, mover engine.StageMover) *engine.Engine {
	t.Helper()
	e, err := engine.NewEngine(engine.Config{
		Resolver:     newResolver(t, repo),
		Applications: apps,
		Mover:        mover,
		Logger:       quietLogger(),
		Now:          func() time.Time { return time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("engine.NewEngine: %v", err)
	}
	return e
}

func TestNewEngine_ValidatesConfig(t *testing.T) {
	t.Parallel()
	repo := rules.NewInMemoryRepository()
	apps := engine.NewInMemoryApplications()
	mover := &recordingMover{}
	resolver, err := rules.NewResolver(repo)
	if err != nil {
		t.Fatalf("rules.NewResolver: %v", err)
	}
	tests := []struct {
		name string
		cfg  engine.Config
	}{
		{name: "nil resolver", cfg: engine.Config{Applications: apps, Mover: mover}},
		{name: "nil applications", cfg: engine.Config{Resolver: resolver, Mover: mover}},
		{name: "nil mover", cfg: engine.Config{Resolver: resolver, Applications: apps}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := engine.NewEngine(tt.cfg); !errors.Is(err, engine.ErrInvalidConfig) {
				t.Errorf("NewEngine err = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestEngine_Handle_AC3_WebchatRuleFiresOnMatchingMessage(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	conversationID := uuid.New()
	messageID := uuid.New()
	ruleID := uuid.New()

	repo := rules.NewInMemoryRepository()
	repo.Seed(rules.Rule{
		ID:            ruleID,
		TenantID:      tenantID,
		Channel:       "webchat",
		Name:          "orçamento → qualificando",
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "orçamento"},
		ActionType:    rules.ActionTypeMoveToStage,
		ActionConfig:  map[string]any{"stage_key": "qualificando"},
		Enabled:       true,
		CreatedAt:     time.Now().Add(-time.Hour),
	})

	apps := engine.NewInMemoryApplications()
	mover := &recordingMover{}
	eng := newEngine(t, repo, apps, mover)

	msg := engine.InboundMessage{
		TenantID:       tenantID,
		ConversationID: conversationID,
		MessageID:      messageID,
		Channel:        "webchat",
		Body:           "Oi! Pode me passar um orçamento?",
		OccurredAt:     time.Now().UTC(),
	}
	if err := eng.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	moves := mover.snapshot()
	if len(moves) != 1 {
		t.Fatalf("expected 1 stage move, got %d", len(moves))
	}
	if moves[0].stageKey != "qualificando" {
		t.Errorf("stageKey = %q, want %q", moves[0].stageKey, "qualificando")
	}
	if moves[0].actor != engine.SystemActor() {
		t.Errorf("actor = %v, want SystemActor()", moves[0].actor)
	}
	if moves[0].reason != engine.MoveReason {
		t.Errorf("reason = %q, want %q", moves[0].reason, engine.MoveReason)
	}

	if apps.Len() != 1 {
		t.Errorf("applications.Len() = %d, want 1", apps.Len())
	}
}

func TestEngine_Handle_AC3_WebchatRuleIgnoredOnWhatsapp(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	repo := rules.NewInMemoryRepository()
	repo.Seed(rules.Rule{
		ID:            uuid.New(),
		TenantID:      tenantID,
		Channel:       "webchat",
		Name:          "orçamento → qualificando (webchat)",
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "orçamento"},
		ActionType:    rules.ActionTypeMoveToStage,
		ActionConfig:  map[string]any{"stage_key": "qualificando"},
		Enabled:       true,
	})

	apps := engine.NewInMemoryApplications()
	mover := &recordingMover{}
	eng := newEngine(t, repo, apps, mover)

	msg := engine.InboundMessage{
		TenantID:       tenantID,
		ConversationID: uuid.New(),
		MessageID:      uuid.New(),
		Channel:        "whatsapp",
		Body:           "Quero um orçamento, por favor.",
		OccurredAt:     time.Now().UTC(),
	}
	if err := eng.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := len(mover.snapshot()); got != 0 {
		t.Fatalf("expected 0 moves on whatsapp channel, got %d", got)
	}
	if apps.Len() != 0 {
		t.Errorf("applications.Len() = %d, want 0", apps.Len())
	}
}

func TestEngine_Handle_IdempotentOnRedelivery(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	conversationID := uuid.New()
	messageID := uuid.New()
	ruleID := uuid.New()

	repo := rules.NewInMemoryRepository()
	repo.Seed(rules.Rule{
		ID:            ruleID,
		TenantID:      tenantID,
		Channel:       "webchat",
		Name:          "orçamento → qualificando",
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "orçamento"},
		ActionType:    rules.ActionTypeMoveToStage,
		ActionConfig:  map[string]any{"stage_key": "qualificando"},
		Enabled:       true,
	})

	apps := engine.NewInMemoryApplications()
	mover := &recordingMover{}
	eng := newEngine(t, repo, apps, mover)

	msg := engine.InboundMessage{
		TenantID:       tenantID,
		ConversationID: conversationID,
		MessageID:      messageID,
		Channel:        "webchat",
		Body:           "orçamento por gentileza",
		OccurredAt:     time.Now().UTC(),
	}
	if err := eng.Handle(context.Background(), msg); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	if err := eng.Handle(context.Background(), msg); err != nil {
		t.Fatalf("second Handle (redelivery): %v", err)
	}
	if got := len(mover.snapshot()); got != 1 {
		t.Errorf("redelivery produced %d moves, want 1 (idempotent)", got)
	}
	if apps.Len() != 1 {
		t.Errorf("applications.Len() = %d, want 1", apps.Len())
	}
}

func TestEngine_Handle_MoverErrorPropagatesForRetry(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	repo := rules.NewInMemoryRepository()
	repo.Seed(rules.Rule{
		ID:            uuid.New(),
		TenantID:      tenantID,
		Channel:       "webchat",
		Name:          "match",
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "olá"},
		ActionType:    rules.ActionTypeMoveToStage,
		ActionConfig:  map[string]any{"stage_key": "novo"},
		Enabled:       true,
	})
	apps := engine.NewInMemoryApplications()
	transientErr := errors.New("pg: connection reset")
	mover := &recordingMover{nextErr: transientErr}
	eng := newEngine(t, repo, apps, mover)

	msg := engine.InboundMessage{
		TenantID:       tenantID,
		ConversationID: uuid.New(),
		MessageID:      uuid.New(),
		Channel:        "webchat",
		Body:           "olá",
		OccurredAt:     time.Now().UTC(),
	}
	err := eng.Handle(context.Background(), msg)
	if err == nil || !strings.Contains(err.Error(), transientErr.Error()) {
		t.Fatalf("Handle err = %v, want wrap of %q", err, transientErr.Error())
	}
	if apps.Len() != 0 {
		t.Errorf("application recorded despite mover failure: Len = %d", apps.Len())
	}
}

func TestEngine_Handle_NoMatchingRulesIsNoop(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	repo := rules.NewInMemoryRepository()
	repo.Seed(rules.Rule{
		ID:            uuid.New(),
		TenantID:      tenantID,
		Channel:       "webchat",
		Name:          "no-match phrase",
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "específica"},
		ActionType:    rules.ActionTypeMoveToStage,
		ActionConfig:  map[string]any{"stage_key": "novo"},
		Enabled:       true,
	})
	apps := engine.NewInMemoryApplications()
	mover := &recordingMover{}
	eng := newEngine(t, repo, apps, mover)

	msg := engine.InboundMessage{
		TenantID:       tenantID,
		ConversationID: uuid.New(),
		MessageID:      uuid.New(),
		Channel:        "webchat",
		Body:           "completely different text",
		OccurredAt:     time.Now().UTC(),
	}
	if err := eng.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := len(mover.snapshot()); got != 0 {
		t.Errorf("expected 0 moves, got %d", got)
	}
	if apps.Len() != 0 {
		t.Errorf("applications.Len() = %d, want 0", apps.Len())
	}
}

func TestEngine_Handle_DisabledRuleIgnored(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	repo := rules.NewInMemoryRepository()
	repo.Seed(rules.Rule{
		ID:            uuid.New(),
		TenantID:      tenantID,
		Channel:       "webchat",
		Name:          "disabled rule",
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "olá"},
		ActionType:    rules.ActionTypeMoveToStage,
		ActionConfig:  map[string]any{"stage_key": "novo"},
		Enabled:       false,
	})
	apps := engine.NewInMemoryApplications()
	mover := &recordingMover{}
	eng := newEngine(t, repo, apps, mover)

	if err := eng.Handle(context.Background(), engine.InboundMessage{
		TenantID:       tenantID,
		ConversationID: uuid.New(),
		MessageID:      uuid.New(),
		Channel:        "webchat",
		Body:           "olá",
		OccurredAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := len(mover.snapshot()); got != 0 {
		t.Errorf("disabled rule fired: %d moves", got)
	}
}

func TestEngine_Handle_RegexMatch(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	repo := rules.NewInMemoryRepository()
	repo.Seed(rules.Rule{
		ID:            uuid.New(),
		TenantID:      tenantID,
		Channel:       "webchat",
		Name:          "regex keyword",
		TriggerType:   rules.TriggerTypeMessageKeywordRegex,
		TriggerConfig: map[string]any{"regex": `(?i)quanto custa`},
		ActionType:    rules.ActionTypeMoveToStage,
		ActionConfig:  map[string]any{"stage_key": "qualificando"},
		Enabled:       true,
	})
	apps := engine.NewInMemoryApplications()
	mover := &recordingMover{}
	eng := newEngine(t, repo, apps, mover)

	if err := eng.Handle(context.Background(), engine.InboundMessage{
		TenantID:       tenantID,
		ConversationID: uuid.New(),
		MessageID:      uuid.New(),
		Channel:        "webchat",
		Body:           "Quanto Custa esse plano?",
		OccurredAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := len(mover.snapshot()); got != 1 {
		t.Errorf("expected 1 move from regex match, got %d", got)
	}
}

func TestEngine_Handle_BadActionConfigSkipsWithoutMove(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	repo := rules.NewInMemoryRepository()
	repo.Seed(rules.Rule{
		ID:            uuid.New(),
		TenantID:      tenantID,
		Channel:       "webchat",
		Name:          "missing stage_key",
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "olá"},
		ActionType:    rules.ActionTypeMoveToStage,
		ActionConfig:  map[string]any{}, // no stage_key, no stage alias
		Enabled:       true,
	})
	apps := engine.NewInMemoryApplications()
	mover := &recordingMover{}
	eng := newEngine(t, repo, apps, mover)

	if err := eng.Handle(context.Background(), engine.InboundMessage{
		TenantID:       tenantID,
		ConversationID: uuid.New(),
		MessageID:      uuid.New(),
		Channel:        "webchat",
		Body:           "olá",
		OccurredAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := len(mover.snapshot()); got != 0 {
		t.Errorf("malformed action config produced %d moves, want 0", got)
	}
	if apps.Len() != 0 {
		t.Errorf("malformed action config produced %d application rows, want 0", apps.Len())
	}
}

func TestEngine_Handle_LegacyStageAliasAccepted(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	repo := rules.NewInMemoryRepository()
	repo.Seed(rules.Rule{
		ID:            uuid.New(),
		TenantID:      tenantID,
		Channel:       "webchat",
		Name:          "legacy stage alias",
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "olá"},
		ActionType:    rules.ActionTypeMoveToStage,
		ActionConfig:  map[string]any{"stage": "qualificando"}, // legacy alias
		Enabled:       true,
	})
	apps := engine.NewInMemoryApplications()
	mover := &recordingMover{}
	eng := newEngine(t, repo, apps, mover)

	if err := eng.Handle(context.Background(), engine.InboundMessage{
		TenantID:       tenantID,
		ConversationID: uuid.New(),
		MessageID:      uuid.New(),
		Channel:        "webchat",
		Body:           "olá",
		OccurredAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	moves := mover.snapshot()
	if len(moves) != 1 || moves[0].stageKey != "qualificando" {
		t.Fatalf("legacy alias not honoured: moves=%+v", moves)
	}
}

func TestEngine_Handle_ApplicationsRaceTreatedAsSuccess(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	repo := rules.NewInMemoryRepository()
	repo.Seed(rules.Rule{
		ID:            uuid.New(),
		TenantID:      tenantID,
		Channel:       "webchat",
		Name:          "match",
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "olá"},
		ActionType:    rules.ActionTypeMoveToStage,
		ActionConfig:  map[string]any{"stage_key": "novo"},
		Enabled:       true,
	})
	mover := &recordingMover{}
	apps := &raceyApplications{} // Record always returns ErrAlreadyApplied
	eng, err := engine.NewEngine(engine.Config{
		Resolver:     newResolver(t, repo),
		Applications: apps,
		Mover:        mover,
		Logger:       quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if err := eng.Handle(context.Background(), engine.InboundMessage{
		TenantID:       tenantID,
		ConversationID: uuid.New(),
		MessageID:      uuid.New(),
		Channel:        "webchat",
		Body:           "olá",
		OccurredAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Handle on race: %v", err)
	}
	// Mover still ran (idempotent); the race resolves to a no-op record.
	if got := len(mover.snapshot()); got != 1 {
		t.Errorf("race-with-applications: moves = %d, want 1", got)
	}
}

func TestEngine_Handle_InvalidInboundMessage(t *testing.T) {
	t.Parallel()

	repo := rules.NewInMemoryRepository()
	apps := engine.NewInMemoryApplications()
	mover := &recordingMover{}
	eng := newEngine(t, repo, apps, mover)

	if err := eng.Handle(context.Background(), engine.InboundMessage{}); !errors.Is(err, engine.ErrInvalidEvent) {
		t.Errorf("Handle on zero message: %v, want ErrInvalidEvent", err)
	}
}

// raceyApplications is a fake [engine.ApplicationsRepo] whose Record
// always reports ErrAlreadyApplied as if a concurrent replica won.
type raceyApplications struct{}

func (raceyApplications) IsApplied(_ context.Context, _, _, _ uuid.UUID) (bool, error) {
	return false, nil
}
func (raceyApplications) Record(_ context.Context, _ engine.Application) error {
	return engine.ErrAlreadyApplied
}
