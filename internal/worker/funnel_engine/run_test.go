package funnel_engine_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/funnel/engine"
	"github.com/pericles-luz/crm/internal/funnel/rules"
	"github.com/pericles-luz/crm/internal/worker/funnel_engine"
)

type fakeSubscriber struct {
	ensureStreamCalls int32
	subscribeCalls    int32
	drainCalls        int32
	ensureErr         error
	subscribeErr      error

	lastEnsureName     string
	lastEnsureSubjects []string
	lastSubject        string
	lastQueue          string
	lastDurable        string
	lastAck            time.Duration
	stored             funnel_engine.HandlerFunc
}

func (s *fakeSubscriber) EnsureStream(name string, subjects []string) error {
	atomic.AddInt32(&s.ensureStreamCalls, 1)
	s.lastEnsureName = name
	s.lastEnsureSubjects = subjects
	return s.ensureErr
}

func (s *fakeSubscriber) Subscribe(_ context.Context, subject, queue, durable string, ack time.Duration, h funnel_engine.HandlerFunc) (funnel_engine.Subscription, error) {
	atomic.AddInt32(&s.subscribeCalls, 1)
	s.lastSubject = subject
	s.lastQueue = queue
	s.lastDurable = durable
	s.lastAck = ack
	s.stored = h
	if s.subscribeErr != nil {
		return nil, s.subscribeErr
	}
	return &fakeSubscription{}, nil
}

func (s *fakeSubscriber) Drain() error {
	atomic.AddInt32(&s.drainCalls, 1)
	return nil
}

type fakeSubscription struct {
	drained int32
}

func (s *fakeSubscription) Drain() error { atomic.AddInt32(&s.drained, 1); return nil }

func buildEngineForRun(t *testing.T) *engine.Engine {
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
		Mover:        noopMover{},
		Logger:       slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

type noopMover struct{}

func (noopMover) MoveConversation(_ context.Context, _, _ uuid.UUID, _ string, _ uuid.UUID, _ string) error {
	return nil
}

func TestRun_ValidatesRequiredArgs(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	e := buildEngineForRun(t)

	cases := []struct {
		name string
		sub  funnel_engine.Subscriber
		cfg  funnel_engine.RunConfig
	}{
		{name: "nil subscriber", sub: nil, cfg: funnel_engine.RunConfig{Engine: e, Logger: logger}},
		{name: "nil engine", sub: &fakeSubscriber{}, cfg: funnel_engine.RunConfig{Logger: logger}},
		{name: "nil logger", sub: &fakeSubscriber{}, cfg: funnel_engine.RunConfig{Engine: e}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := funnel_engine.Run(context.Background(), tc.sub, tc.cfg); err == nil {
				t.Errorf("Run want error, got nil")
			}
		})
	}
}

func TestRun_EnsureStreamFailsBubbles(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	sub := &fakeSubscriber{ensureErr: errors.New("boom")}
	err := funnel_engine.Run(context.Background(), sub, funnel_engine.RunConfig{
		Engine: buildEngineForRun(t),
		Logger: logger,
	})
	if err == nil {
		t.Fatal("Run: want error from EnsureStream")
	}
	if atomic.LoadInt32(&sub.ensureStreamCalls) != 1 {
		t.Errorf("EnsureStream calls = %d, want 1", sub.ensureStreamCalls)
	}
}

func TestRun_SubscribeFailsBubbles(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	sub := &fakeSubscriber{subscribeErr: errors.New("subscribe boom")}
	err := funnel_engine.Run(context.Background(), sub, funnel_engine.RunConfig{
		Engine: buildEngineForRun(t),
		Logger: logger,
	})
	if err == nil {
		t.Fatal("Run: want error from Subscribe")
	}
}

func TestRun_HappyPathWiresEngineSubjectsAndDrains(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	sub := &fakeSubscriber{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- funnel_engine.Run(ctx, sub, funnel_engine.RunConfig{
			Engine:  buildEngineForRun(t),
			Logger:  logger,
			AckWait: 5 * time.Second,
		})
	}()
	// Give Run a tick to set up.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned err = %v, want nil after ctx.Done()", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
	if sub.lastSubject != engine.Subject {
		t.Errorf("subject = %q, want %q", sub.lastSubject, engine.Subject)
	}
	if sub.lastQueue != engine.QueueName {
		t.Errorf("queue = %q, want %q", sub.lastQueue, engine.QueueName)
	}
	if sub.lastDurable != engine.DurableName {
		t.Errorf("durable = %q, want %q", sub.lastDurable, engine.DurableName)
	}
	if sub.lastEnsureName != engine.StreamName {
		t.Errorf("stream = %q, want %q", sub.lastEnsureName, engine.StreamName)
	}
	if sub.lastAck != 5*time.Second {
		t.Errorf("ackWait = %v, want %v", sub.lastAck, 5*time.Second)
	}
	if atomic.LoadInt32(&sub.drainCalls) != 1 {
		t.Errorf("subscriber Drain calls = %d, want 1", sub.drainCalls)
	}
}

func TestRun_DefaultAckWaitApplies(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	sub := &fakeSubscriber{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- funnel_engine.Run(ctx, sub, funnel_engine.RunConfig{
			Engine: buildEngineForRun(t),
			Logger: logger,
		})
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	if sub.lastAck != funnel_engine.DefaultAckWait {
		t.Errorf("ackWait = %v, want %v", sub.lastAck, funnel_engine.DefaultAckWait)
	}
}
