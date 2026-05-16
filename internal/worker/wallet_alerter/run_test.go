package wallet_alerter_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/worker/wallet_alerter"
)

// fakeSubscription satisfies wallet_alerter.Subscription.
type fakeSubscription struct {
	drained atomic.Bool
}

func (s *fakeSubscription) Drain() error { s.drained.Store(true); return nil }

// fakeSubscriber satisfies wallet_alerter.Subscriber for unit tests.
type fakeSubscriber struct {
	mu              sync.Mutex
	ensureCalls     [][]string // (name, subjects)
	ensureErr       error
	subscribeErr    error
	subscribeCalled atomic.Bool
	handler         wallet_alerter.HandlerFunc
	subject         string
	queue           string
	durable         string
	ackWait         time.Duration
	drained         atomic.Bool
}

func (s *fakeSubscriber) EnsureStream(name string, subjects []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := append([]string{name}, subjects...)
	s.ensureCalls = append(s.ensureCalls, rec)
	return s.ensureErr
}

func (s *fakeSubscriber) Subscribe(
	_ context.Context,
	subject, queue, durable string,
	ackWait time.Duration,
	handler wallet_alerter.HandlerFunc,
) (wallet_alerter.Subscription, error) {
	if s.subscribeErr != nil {
		return nil, s.subscribeErr
	}
	s.mu.Lock()
	s.subject = subject
	s.queue = queue
	s.durable = durable
	s.ackWait = ackWait
	s.handler = handler
	s.mu.Unlock()
	s.subscribeCalled.Store(true)
	return &fakeSubscription{}, nil
}

func (s *fakeSubscriber) Drain() error { s.drained.Store(true); return nil }

// ---------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------

func TestRun_RequiresSubscriber(t *testing.T) {
	t.Parallel()
	err := wallet_alerter.Run(context.Background(), nil, wallet_alerter.RunConfig{
		Notifier: &recordingNotifier{},
		Logger:   silentLogger(),
	})
	if err == nil {
		t.Fatal("expected error when Subscriber is nil")
	}
}

func TestRun_RequiresNotifier(t *testing.T) {
	t.Parallel()
	err := wallet_alerter.Run(context.Background(), &fakeSubscriber{}, wallet_alerter.RunConfig{
		Logger: silentLogger(),
	})
	if err == nil {
		t.Fatal("expected error when Notifier is nil")
	}
}

func TestRun_RequiresLogger(t *testing.T) {
	t.Parallel()
	err := wallet_alerter.Run(context.Background(), &fakeSubscriber{}, wallet_alerter.RunConfig{
		Notifier: &recordingNotifier{},
	})
	if err == nil {
		t.Fatal("expected error when Logger is nil")
	}
}

// ---------------------------------------------------------------------
// Failure modes
// ---------------------------------------------------------------------

func TestRun_PropagatesEnsureStreamError(t *testing.T) {
	t.Parallel()
	sub := &fakeSubscriber{ensureErr: errors.New("boom")}
	err := wallet_alerter.Run(context.Background(), sub, wallet_alerter.RunConfig{
		Notifier: &recordingNotifier{},
		Logger:   silentLogger(),
	})
	if err == nil {
		t.Fatal("expected EnsureStream error to bubble up")
	}
	if !strings.Contains(err.Error(), "ensure stream") {
		t.Errorf("error not wrapped with stage label: %v", err)
	}
}

func TestRun_PropagatesSubscribeError(t *testing.T) {
	t.Parallel()
	sub := &fakeSubscriber{subscribeErr: errors.New("nope")}
	err := wallet_alerter.Run(context.Background(), sub, wallet_alerter.RunConfig{
		Notifier: &recordingNotifier{},
		Logger:   silentLogger(),
	})
	if err == nil {
		t.Fatal("expected Subscribe error to bubble up")
	}
	if !strings.Contains(err.Error(), "subscribe") {
		t.Errorf("error not wrapped with stage label: %v", err)
	}
}

// ---------------------------------------------------------------------
// Boot wiring
// ---------------------------------------------------------------------

func TestRun_WiresStreamAndSubscription(t *testing.T) {
	t.Parallel()
	sub := &fakeSubscriber{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- wallet_alerter.Run(ctx, sub, wallet_alerter.RunConfig{
			Notifier: &recordingNotifier{},
			Logger:   silentLogger(),
		})
	}()

	// Spin until Subscribe has been called, then cancel for a clean
	// shutdown. A bounded sleep loop is acceptable here because Run
	// blocks on ctx.Done() — we cannot synchronise without polling.
	deadline := time.Now().Add(2 * time.Second)
	for !sub.subscribeCalled.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !sub.subscribeCalled.Load() {
		cancel()
		<-done
		t.Fatal("Subscribe was not called within 2s")
	}

	if got, want := sub.subject, wallet_alerter.Subject; got != want {
		t.Errorf("subject = %q, want %q", got, want)
	}
	if got, want := sub.queue, wallet_alerter.QueueName; got != want {
		t.Errorf("queue = %q, want %q", got, want)
	}
	if got, want := sub.durable, wallet_alerter.DurableName; got != want {
		t.Errorf("durable = %q, want %q", got, want)
	}
	if sub.ackWait <= 0 {
		t.Errorf("ackWait must default to a positive value; got %s", sub.ackWait)
	}
	if len(sub.ensureCalls) != 1 || sub.ensureCalls[0][0] != wallet_alerter.StreamName {
		t.Errorf("EnsureStream not invoked with %q: got %+v", wallet_alerter.StreamName, sub.ensureCalls)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned error: %v", err)
	}
	if !sub.drained.Load() {
		t.Error("Subscriber.Drain was not called on shutdown")
	}
}

// ---------------------------------------------------------------------
// Degraded-notifier warning (AC #3)
// ---------------------------------------------------------------------

func TestRun_DegradedNotifier_LogsWarning(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub := &fakeSubscriber{}
	done := make(chan error, 1)
	go func() {
		done <- wallet_alerter.Run(ctx, sub, wallet_alerter.RunConfig{
			Notifier:       &recordingNotifier{},
			NotifyDegraded: true,
			Logger:         captureLogger(buf),
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !sub.subscribeCalled.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	out := buf.String()
	if !strings.Contains(out, "SLACK_ALERTS_WEBHOOK_URL not configured") {
		t.Errorf("expected degraded-notifier warning in log output, got:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected WARN level for degraded-notifier message, got:\n%s", out)
	}
}
