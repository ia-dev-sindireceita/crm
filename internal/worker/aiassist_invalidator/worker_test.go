package aiassistinvalidator_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	aiassistinvalidator "github.com/pericles-luz/crm/internal/worker/aiassist_invalidator"
)

// fakeSubscriber implements EventSubscriber. Tests push deliveries onto
// the internal channel via Deliver.
type fakeSubscriber struct {
	deliveries chan aiassistinvalidator.Delivery
	errs       chan error
	subErr     error
}

func newFakeSubscriber() *fakeSubscriber {
	return &fakeSubscriber{
		deliveries: make(chan aiassistinvalidator.Delivery, 8),
		errs:       make(chan error, 1),
	}
}

func (f *fakeSubscriber) Subscribe(_ context.Context) (<-chan aiassistinvalidator.Delivery, <-chan error, error) {
	if f.subErr != nil {
		return nil, nil, f.subErr
	}
	return f.deliveries, f.errs, nil
}

func (f *fakeSubscriber) deliver(d aiassistinvalidator.Delivery) { f.deliveries <- d }
func (f *fakeSubscriber) close()                                 { close(f.deliveries) }

// fakeDelivery is a controllable Delivery.
type fakeDelivery struct {
	data    []byte
	msgID   string
	acked   bool
	naked   bool
	nakWait time.Duration
}

func (d *fakeDelivery) Data() []byte                { return d.data }
func (d *fakeDelivery) MsgID() string               { return d.msgID }
func (d *fakeDelivery) Ack(_ context.Context) error { d.acked = true; return nil }
func (d *fakeDelivery) Nak(_ context.Context, w time.Duration) error {
	d.naked = true
	d.nakWait = w
	return nil
}

// fakeInvalidator records invocations.
type fakeInvalidator struct {
	mu     sync.Mutex
	calls  []invalidateCall
	err    error
	called chan struct{}
}

type invalidateCall struct {
	tenantID       uuid.UUID
	conversationID uuid.UUID
}

func newFakeInvalidator() *fakeInvalidator {
	return &fakeInvalidator{called: make(chan struct{}, 8)}
}

func (f *fakeInvalidator) Invalidate(_ context.Context, tenantID, conversationID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, invalidateCall{tenantID: tenantID, conversationID: conversationID})
	select {
	case f.called <- struct{}{}:
	default:
	}
	return f.err
}

func TestNew_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	if _, err := aiassistinvalidator.New(aiassistinvalidator.Config{}); err == nil {
		t.Fatal("expected error for missing subscriber")
	}
	if _, err := aiassistinvalidator.New(aiassistinvalidator.Config{Subscriber: newFakeSubscriber()}); err == nil {
		t.Fatal("expected error for missing invalidator")
	}
	if _, err := aiassistinvalidator.New(aiassistinvalidator.Config{Subscriber: newFakeSubscriber(), Invalidator: newFakeInvalidator()}); err != nil {
		t.Fatalf("unexpected error on minimal valid config: %v", err)
	}
}

func TestHandle_InvalidatesAndAcksOnSuccess(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	body, _ := json.Marshal(aiassistinvalidator.Event{TenantID: tenant, ConversationID: conv, MessageID: uuid.New()})
	d := &fakeDelivery{data: body, msgID: "msg-1"}
	inv := newFakeInvalidator()
	w, err := aiassistinvalidator.New(aiassistinvalidator.Config{
		Subscriber:  newFakeSubscriber(),
		Invalidator: inv,
		Metrics:     aiassistinvalidator.NewMetrics(nil),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.Handle(context.Background(), d)

	if !d.acked {
		t.Errorf("delivery not acked on success")
	}
	if d.naked {
		t.Errorf("delivery should not be Nak'd on success")
	}
	if len(inv.calls) != 1 {
		t.Fatalf("invalidate calls: got %d want 1", len(inv.calls))
	}
	if inv.calls[0].tenantID != tenant || inv.calls[0].conversationID != conv {
		t.Errorf("invalidate args: got (%s, %s) want (%s, %s)", inv.calls[0].tenantID, inv.calls[0].conversationID, tenant, conv)
	}
}

func TestHandle_AcksOnDecodeFailure(t *testing.T) {
	t.Parallel()
	d := &fakeDelivery{data: []byte("not-json"), msgID: "msg-bad"}
	inv := newFakeInvalidator()
	w, _ := aiassistinvalidator.New(aiassistinvalidator.Config{
		Subscriber:  newFakeSubscriber(),
		Invalidator: inv,
		Metrics:     aiassistinvalidator.NewMetrics(nil),
	})
	w.Handle(context.Background(), d)

	if !d.acked {
		t.Errorf("decode failure must Ack to avoid poison-pill loop")
	}
	if len(inv.calls) != 0 {
		t.Errorf("invalidate should not be called on decode failure")
	}
}

func TestHandle_AcksOnMissingIDs(t *testing.T) {
	t.Parallel()
	body, _ := json.Marshal(aiassistinvalidator.Event{})
	d := &fakeDelivery{data: body, msgID: "msg-empty"}
	inv := newFakeInvalidator()
	w, _ := aiassistinvalidator.New(aiassistinvalidator.Config{
		Subscriber:  newFakeSubscriber(),
		Invalidator: inv,
	})
	w.Handle(context.Background(), d)
	if !d.acked {
		t.Errorf("missing ids must Ack to drop poison-pill")
	}
	if len(inv.calls) != 0 {
		t.Errorf("invalidate must not be called when ids are zero")
	}
}

func TestHandle_NaksOnInvalidateFailure(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	body, _ := json.Marshal(aiassistinvalidator.Event{TenantID: tenant, ConversationID: conv})
	d := &fakeDelivery{data: body, msgID: "msg-retry"}
	inv := newFakeInvalidator()
	inv.err = errors.New("transient")
	w, _ := aiassistinvalidator.New(aiassistinvalidator.Config{
		Subscriber:  newFakeSubscriber(),
		Invalidator: inv,
		NakDelay:    2 * time.Second,
	})
	w.Handle(context.Background(), d)
	if d.acked {
		t.Errorf("must not Ack on invalidate failure")
	}
	if !d.naked {
		t.Fatalf("must Nak on invalidate failure")
	}
	if d.nakWait != 2*time.Second {
		t.Errorf("nak wait: got %v want 2s", d.nakWait)
	}
}

func TestRun_PropagatesSubscribeError(t *testing.T) {
	t.Parallel()
	sub := newFakeSubscriber()
	sub.subErr = errors.New("subscribe boom")
	w, _ := aiassistinvalidator.New(aiassistinvalidator.Config{
		Subscriber:  sub,
		Invalidator: newFakeInvalidator(),
	})
	err := w.Run(context.Background())
	if err == nil || !errIsWrapped(err, "subscribe boom") {
		t.Fatalf("Run error: got %v, want wrapping subscribe boom", err)
	}
}

func TestRun_DrainsThenReturnsOnSubscriberClose(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	body, _ := json.Marshal(aiassistinvalidator.Event{TenantID: tenant, ConversationID: conv})
	sub := newFakeSubscriber()
	inv := newFakeInvalidator()
	w, _ := aiassistinvalidator.New(aiassistinvalidator.Config{
		Subscriber:  sub,
		Invalidator: inv,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	sub.deliver(&fakeDelivery{data: body, msgID: "ac6-1"})
	select {
	case <-inv.called:
	case <-time.After(time.Second):
		t.Fatalf("invalidator never invoked")
	}
	sub.close()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v after subscriber close", err)
	}
	if len(inv.calls) != 1 {
		t.Fatalf("invalidate calls: got %d want 1", len(inv.calls))
	}
}

// errIsWrapped reports whether s is contained in err.Error(). Cheap
// substring check is enough — we don't need full unwrap traversal.
func errIsWrapped(err error, s string) bool {
	if err == nil {
		return false
	}
	return contains(err.Error(), s)
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
