package funnel_engine_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/funnel/engine"
	"github.com/pericles-luz/crm/internal/funnel/rules"
	"github.com/pericles-luz/crm/internal/worker/funnel_engine"
)

type stubDelivery struct {
	data     []byte
	ackCalls int32
	ackErr   error
}

func (d *stubDelivery) Data() []byte { return d.data }
func (d *stubDelivery) Ack(_ context.Context) error {
	atomic.AddInt32(&d.ackCalls, 1)
	return d.ackErr
}

func (d *stubDelivery) ackCount() int32 { return atomic.LoadInt32(&d.ackCalls) }

type stubMover struct {
	mu    sync.Mutex
	moves []moveCall
	err   error
}

type moveCall struct {
	tenantID       uuid.UUID
	conversationID uuid.UUID
	stage          string
}

func (m *stubMover) MoveConversation(_ context.Context, t, c uuid.UUID, stage string, _ uuid.UUID, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.moves = append(m.moves, moveCall{tenantID: t, conversationID: c, stage: stage})
	return nil
}

func (m *stubMover) snapshot() []moveCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]moveCall, len(m.moves))
	copy(out, m.moves)
	return out
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func buildEngine(t *testing.T, mover engine.StageMover) (*engine.Engine, *rules.InMemoryRepository, *engine.InMemoryApplications) {
	t.Helper()
	repo := rules.NewInMemoryRepository()
	apps := engine.NewInMemoryApplications()
	resolver, err := rules.NewResolver(repo)
	if err != nil {
		t.Fatalf("rules.NewResolver: %v", err)
	}
	e, err := engine.NewEngine(engine.Config{
		Resolver:     resolver,
		Applications: apps,
		Mover:        mover,
		Logger:       quietLogger(),
	})
	if err != nil {
		t.Fatalf("engine.NewEngine: %v", err)
	}
	return e, repo, apps
}

func TestConsumer_NilEngineRejected(t *testing.T) {
	t.Parallel()
	if _, err := funnel_engine.NewConsumer(nil, quietLogger()); err == nil {
		t.Error("NewConsumer(nil engine): want error")
	}
}

func TestConsumer_Handle_AcksMalformedPayload(t *testing.T) {
	t.Parallel()
	mover := &stubMover{}
	e, _, _ := buildEngine(t, mover)
	c, err := funnel_engine.NewConsumer(e, quietLogger())
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	d := &stubDelivery{data: []byte("not json")}
	if err := c.Handle(context.Background(), d); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if d.ackCount() != 1 {
		t.Errorf("Ack count = %d, want 1 (poison ack)", d.ackCount())
	}
	if len(mover.snapshot()) != 0 {
		t.Error("malformed payload should not reach mover")
	}
}

func TestConsumer_Handle_AcksEmptyPayload(t *testing.T) {
	t.Parallel()
	mover := &stubMover{}
	e, _, _ := buildEngine(t, mover)
	c, err := funnel_engine.NewConsumer(e, quietLogger())
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	d := &stubDelivery{data: nil}
	if err := c.Handle(context.Background(), d); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if d.ackCount() != 1 {
		t.Errorf("Ack count = %d, want 1", d.ackCount())
	}
}

func TestConsumer_Handle_AppliesRuleAndAcks(t *testing.T) {
	t.Parallel()
	mover := &stubMover{}
	e, repo, apps := buildEngine(t, mover)

	tenantID := uuid.New()
	repo.Seed(rules.Rule{
		ID:            uuid.New(),
		TenantID:      tenantID,
		Channel:       "webchat",
		Name:          "orçamento → qualificando",
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "orçamento"},
		ActionType:    rules.ActionTypeMoveToStage,
		ActionConfig:  map[string]any{"stage_key": "qualificando"},
		Enabled:       true,
	})

	body, err := engine.EncodeInboundMessage(engine.InboundMessage{
		TenantID:       tenantID,
		ConversationID: uuid.New(),
		MessageID:      uuid.New(),
		Channel:        "webchat",
		Body:           "preciso de orçamento",
		OccurredAt:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	c, err := funnel_engine.NewConsumer(e, quietLogger())
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	d := &stubDelivery{data: body}
	if err := c.Handle(context.Background(), d); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if d.ackCount() != 1 {
		t.Errorf("Ack count = %d, want 1", d.ackCount())
	}
	if len(mover.snapshot()) != 1 {
		t.Fatalf("expected 1 move, got %+v", mover.snapshot())
	}
	if apps.Len() != 1 {
		t.Errorf("applications.Len = %d, want 1", apps.Len())
	}
}

func TestConsumer_Handle_TransientErrorReturnsForRetry(t *testing.T) {
	t.Parallel()
	mover := &stubMover{err: errors.New("pg: connection reset")}
	e, repo, _ := buildEngine(t, mover)

	tenantID := uuid.New()
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

	body, _ := engine.EncodeInboundMessage(engine.InboundMessage{
		TenantID:       tenantID,
		ConversationID: uuid.New(),
		MessageID:      uuid.New(),
		Channel:        "webchat",
		Body:           "olá",
		OccurredAt:     time.Now().UTC(),
	})
	c, _ := funnel_engine.NewConsumer(e, quietLogger())
	d := &stubDelivery{data: body}
	err := c.Handle(context.Background(), d)
	if err == nil {
		t.Fatal("expected error to surface for redelivery")
	}
	if d.ackCount() != 0 {
		t.Errorf("transient error should NOT ack; got Ack count = %d", d.ackCount())
	}
}

func TestConsumer_NilDeliveryRejected(t *testing.T) {
	t.Parallel()
	mover := &stubMover{}
	e, _, _ := buildEngine(t, mover)
	c, _ := funnel_engine.NewConsumer(e, quietLogger())
	if err := c.Handle(context.Background(), nil); err == nil {
		t.Error("Handle(nil) should error")
	}
}
