package wallet_alerter_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/worker/wallet_alerter"
)

// recordingNotifier captures every Notify call. errOnNext, when set,
// is returned from the next Notify and then cleared so successive
// calls succeed.
type recordingNotifier struct {
	mu        sync.Mutex
	calls     []string
	errOnNext error
}

func (n *recordingNotifier) Notify(_ context.Context, msg string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.errOnNext != nil {
		err := n.errOnNext
		n.errOnNext = nil
		return err
	}
	n.calls = append(n.calls, msg)
	return nil
}

func (n *recordingNotifier) Calls() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	cp := make([]string, len(n.calls))
	copy(cp, n.calls)
	return cp
}

// fakeDelivery satisfies wallet_alerter.Delivery for unit tests.
type fakeDelivery struct {
	data    []byte
	acked   atomic.Bool
	ackErr  error
	ackHook func()
}

func (d *fakeDelivery) Data() []byte { return d.data }

func (d *fakeDelivery) Ack(_ context.Context) error {
	d.acked.Store(true)
	if d.ackHook != nil {
		d.ackHook()
	}
	return d.ackErr
}

// fakeClock is a manual clock for dedup TTL tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// silentLogger returns a slog logger that writes to io.Discard so test
// output stays readable. We do not assert on log lines (slog handler
// internals are not a stable test surface); behavioural assertions
// happen on the Notifier and the delivery ACK.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// captureLogger returns a logger that writes into the provided buffer.
// Used by the few tests that DO need to verify a specific warning is
// surfaced (e.g. the degraded-notifier boot warning).
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func newAlerter(t *testing.T, n wallet_alerter.Notifier, clk wallet_alerter.Clock, ttl time.Duration) *wallet_alerter.Alerter {
	t.Helper()
	a, err := wallet_alerter.New(n, silentLogger(), clk, ttl)
	if err != nil {
		t.Fatalf("wallet_alerter.New: %v", err)
	}
	return a
}

const validEventJSON = `{
  "tenant_id": "0190d111-1111-7111-8111-111111111111",
  "policy_scope": "tenant:default",
  "last_charge_tokens": 1234,
  "occurred_at": "2026-05-16T19:00:00Z"
}`

// ---------------------------------------------------------------------
// Constructor contract
// ---------------------------------------------------------------------

func TestNew_RequiresNotifier(t *testing.T) {
	t.Parallel()
	if _, err := wallet_alerter.New(nil, silentLogger(), nil, 0); err == nil {
		t.Fatal("expected error when Notifier is nil")
	}
}

func TestNew_RequiresLogger(t *testing.T) {
	t.Parallel()
	if _, err := wallet_alerter.New(&recordingNotifier{}, nil, nil, 0); err == nil {
		t.Fatal("expected error when logger is nil")
	}
}

// ---------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------

func TestHandle_ValidEvent_PostsOnceAndAcks(t *testing.T) {
	t.Parallel()
	n := &recordingNotifier{}
	clk := &fakeClock{now: time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)}
	a := newAlerter(t, n, clk, 0)
	d := &fakeDelivery{data: []byte(validEventJSON)}

	if err := a.Handle(context.Background(), d); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !d.acked.Load() {
		t.Fatal("delivery not acked")
	}
	calls := n.Calls()
	if len(calls) != 1 {
		t.Fatalf("Notify calls = %d, want 1", len(calls))
	}
	const want = ":warning: Wallet zerada em tenant `0190d111-1111-7111-8111-111111111111` (escopo `tenant:default`). Último débito: 1234 tokens em 2026-05-16T19:00:00Z."
	if calls[0] != want {
		t.Errorf("Notify body mismatch:\n got: %s\nwant: %s", calls[0], want)
	}
}

// ---------------------------------------------------------------------
// Dedup
// ---------------------------------------------------------------------

func TestHandle_DuplicateEvent_PostsOnce(t *testing.T) {
	t.Parallel()
	n := &recordingNotifier{}
	clk := &fakeClock{now: time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)}
	a := newAlerter(t, n, clk, time.Hour)

	for i := 0; i < 5; i++ {
		d := &fakeDelivery{data: []byte(validEventJSON)}
		if err := a.Handle(context.Background(), d); err != nil {
			t.Fatalf("Handle #%d: %v", i, err)
		}
		if !d.acked.Load() {
			t.Errorf("delivery #%d not acked", i)
		}
	}
	if got := len(n.Calls()); got != 1 {
		t.Errorf("Notify calls = %d, want 1 (dedup must collapse identical events)", got)
	}
}

func TestHandle_DedupExpiresAfterTTL(t *testing.T) {
	t.Parallel()
	n := &recordingNotifier{}
	clk := &fakeClock{now: time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)}
	a := newAlerter(t, n, clk, time.Hour)

	first := &fakeDelivery{data: []byte(validEventJSON)}
	if err := a.Handle(context.Background(), first); err != nil {
		t.Fatalf("Handle first: %v", err)
	}

	// Advance the clock past the TTL. The same key MUST now bypass the
	// dedup and produce a second POST.
	clk.Advance(time.Hour + time.Minute)

	second := &fakeDelivery{data: []byte(validEventJSON)}
	if err := a.Handle(context.Background(), second); err != nil {
		t.Fatalf("Handle second: %v", err)
	}
	if got := len(n.Calls()); got != 2 {
		t.Errorf("Notify calls = %d, want 2 (dedup must expire after TTL)", got)
	}
}

func TestHandle_DifferentOccurredAt_IndependentDedup(t *testing.T) {
	t.Parallel()
	n := &recordingNotifier{}
	clk := &fakeClock{now: time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)}
	a := newAlerter(t, n, clk, time.Hour)

	const evA = `{"tenant_id":"t1","policy_scope":"s","last_charge_tokens":1,"occurred_at":"2026-05-16T19:00:00Z"}`
	const evB = `{"tenant_id":"t1","policy_scope":"s","last_charge_tokens":2,"occurred_at":"2026-05-16T19:30:00Z"}`

	for _, body := range []string{evA, evB, evA, evB} {
		d := &fakeDelivery{data: []byte(body)}
		if err := a.Handle(context.Background(), d); err != nil {
			t.Fatalf("Handle %q: %v", body, err)
		}
	}
	if got := len(n.Calls()); got != 2 {
		t.Errorf("Notify calls = %d, want 2 (one per distinct occurred_at)", got)
	}
}

// ---------------------------------------------------------------------
// Poison handling
// ---------------------------------------------------------------------

func TestHandle_MalformedJSON_AcksWithoutNotify(t *testing.T) {
	t.Parallel()
	n := &recordingNotifier{}
	a := newAlerter(t, n, nil, 0)

	d := &fakeDelivery{data: []byte(`{not valid`)}
	if err := a.Handle(context.Background(), d); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !d.acked.Load() {
		t.Error("malformed payload must be acked (poison)")
	}
	if got := len(n.Calls()); got != 0 {
		t.Errorf("Notify calls = %d, want 0", got)
	}
}

func TestHandle_EmptyBody_AcksWithoutNotify(t *testing.T) {
	t.Parallel()
	n := &recordingNotifier{}
	a := newAlerter(t, n, nil, 0)
	d := &fakeDelivery{data: nil}
	if err := a.Handle(context.Background(), d); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !d.acked.Load() {
		t.Error("empty body must still be acked (poison)")
	}
	if got := len(n.Calls()); got != 0 {
		t.Errorf("Notify calls = %d, want 0", got)
	}
}

func TestHandle_MissingTenantID_AcksWithoutNotify(t *testing.T) {
	t.Parallel()
	n := &recordingNotifier{}
	a := newAlerter(t, n, nil, 0)
	d := &fakeDelivery{data: []byte(`{"policy_scope":"s","last_charge_tokens":1,"occurred_at":"2026-05-16T19:00:00Z"}`)}
	if err := a.Handle(context.Background(), d); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !d.acked.Load() {
		t.Error("missing tenant_id must be acked (poison)")
	}
	if got := len(n.Calls()); got != 0 {
		t.Errorf("Notify calls = %d, want 0", got)
	}
}

func TestHandle_MissingOccurredAt_AcksWithoutNotify(t *testing.T) {
	t.Parallel()
	n := &recordingNotifier{}
	a := newAlerter(t, n, nil, 0)
	d := &fakeDelivery{data: []byte(`{"tenant_id":"t","policy_scope":"s","last_charge_tokens":1}`)}
	if err := a.Handle(context.Background(), d); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !d.acked.Load() {
		t.Error("missing occurred_at must be acked (poison)")
	}
	if got := len(n.Calls()); got != 0 {
		t.Errorf("Notify calls = %d, want 0", got)
	}
}

func TestHandle_NilDelivery_ReturnsError(t *testing.T) {
	t.Parallel()
	a := newAlerter(t, &recordingNotifier{}, nil, 0)
	if err := a.Handle(context.Background(), nil); err == nil {
		t.Fatal("expected error on nil delivery")
	}
}

// ---------------------------------------------------------------------
// Notifier failure semantics
// ---------------------------------------------------------------------

func TestHandle_NotifyFails_ReturnsErrorAndSkipsDedup(t *testing.T) {
	t.Parallel()
	n := &recordingNotifier{errOnNext: errors.New("slack: webhook returned status 503")}
	clk := &fakeClock{now: time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)}
	a := newAlerter(t, n, clk, time.Hour)

	// First attempt: Notify fails.
	first := &fakeDelivery{data: []byte(validEventJSON)}
	err := a.Handle(context.Background(), first)
	if err == nil {
		t.Fatal("expected error from failed Notify")
	}
	if first.acked.Load() {
		t.Error("delivery must NOT be acked when Notify fails (JetStream needs to redeliver)")
	}
	if got := len(n.Calls()); got != 0 {
		t.Fatalf("expected 0 successful calls after failure, got %d", got)
	}

	// Second attempt (redelivery): Notify now succeeds. The dedup must
	// NOT have recorded the first attempt, so this call lands.
	second := &fakeDelivery{data: []byte(validEventJSON)}
	if err := a.Handle(context.Background(), second); err != nil {
		t.Fatalf("Handle (redelivery): %v", err)
	}
	if !second.acked.Load() {
		t.Error("redelivery must be acked on success")
	}
	if got := len(n.Calls()); got != 1 {
		t.Errorf("expected 1 successful call after redelivery, got %d", got)
	}
}
