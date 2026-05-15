package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/media/scanner"
	"github.com/pericles-luz/crm/internal/media/worker"
)

// --- fakes -----------------------------------------------------------

type fakeScanner struct {
	result scanner.ScanResult
	err    error
	calls  atomic.Int32
}

func (f *fakeScanner) Scan(_ context.Context, _ string) (scanner.ScanResult, error) {
	f.calls.Add(1)
	return f.result, f.err
}

type fakeStore struct {
	mu       sync.Mutex
	err      error
	received []scanner.ScanResult
	tenantID uuid.UUID
	msgID    uuid.UUID
}

func (f *fakeStore) UpdateScanResult(_ context.Context, tenantID, messageID uuid.UUID, r scanner.ScanResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = append(f.received, r)
	f.tenantID = tenantID
	f.msgID = messageID
	return f.err
}

type fakePublisher struct {
	mu     sync.Mutex
	err    error
	bodies [][]byte
	subs   []string
}

func (f *fakePublisher) Publish(_ context.Context, subject string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, subject)
	f.bodies = append(f.bodies, body)
	return f.err
}

type fakeDelivery struct {
	body   []byte
	acked  atomic.Int32
	ackErr error
}

func (f *fakeDelivery) Data() []byte { return f.body }
func (f *fakeDelivery) Ack(_ context.Context) error {
	f.acked.Add(1)
	return f.ackErr
}

// --- helpers ---------------------------------------------------------

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newHandler(t *testing.T, s scanner.MediaScanner, store scanner.MessageMediaStore, pub worker.Publisher) *worker.Handler {
	t.Helper()
	h, err := worker.New(s, store, pub, quietLogger())
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	return h
}

func makeReq(t *testing.T, key string) []byte {
	t.Helper()
	b, err := json.Marshal(worker.Request{
		TenantID:  uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		MessageID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Key:       key,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// --- tests -----------------------------------------------------------

func TestNew_RequiresCollaborators(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		s    scanner.MediaScanner
		st   scanner.MessageMediaStore
		p    worker.Publisher
	}{
		{"nil scanner", nil, &fakeStore{}, &fakePublisher{}},
		{"nil store", &fakeScanner{}, nil, &fakePublisher{}},
		{"nil publisher", &fakeScanner{}, &fakeStore{}, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := worker.New(tc.s, tc.st, tc.p, nil); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestNew_DefaultsLogger(t *testing.T) {
	t.Parallel()
	h, err := worker.New(&fakeScanner{}, &fakeStore{}, &fakePublisher{}, nil)
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	if h.Logger == nil {
		t.Fatal("Logger should default to slog.Default")
	}
}

func TestHandle_HappyPath_CleanVerdict(t *testing.T) {
	t.Parallel()
	s := &fakeScanner{result: scanner.ScanResult{Status: scanner.StatusClean, EngineID: "clamav-1.2.3"}}
	store := &fakeStore{}
	pub := &fakePublisher{}
	h := newHandler(t, s, store, pub)

	msg := &fakeDelivery{body: makeReq(t, "media/abc")}
	if err := h.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if msg.acked.Load() != 1 {
		t.Fatalf("expected exactly one ack, got %d", msg.acked.Load())
	}
	if len(store.received) != 1 || store.received[0].Status != scanner.StatusClean {
		t.Fatalf("store did not receive clean verdict: %+v", store.received)
	}
	if len(pub.bodies) != 1 || pub.subs[0] != worker.SubjectCompleted {
		t.Fatalf("expected publish on completed subject, got subs=%v", pub.subs)
	}
	var got worker.Completed
	if err := json.Unmarshal(pub.bodies[0], &got); err != nil {
		t.Fatalf("unmarshal completed: %v", err)
	}
	if got.Status != scanner.StatusClean || got.EngineID != "clamav-1.2.3" {
		t.Fatalf("completed payload wrong: %+v", got)
	}
}

func TestHandle_InfectedVerdict_Persisted(t *testing.T) {
	t.Parallel()
	s := &fakeScanner{result: scanner.ScanResult{Status: scanner.StatusInfected, EngineID: "clamav-1.2.3"}}
	store := &fakeStore{}
	pub := &fakePublisher{}
	h := newHandler(t, s, store, pub)

	msg := &fakeDelivery{body: makeReq(t, "media/eicar")}
	if err := h.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if store.received[0].Status != scanner.StatusInfected {
		t.Fatalf("expected infected, got %v", store.received[0].Status)
	}
	if msg.acked.Load() != 1 {
		t.Fatalf("expected ack on infected verdict")
	}
}

func TestHandle_BadJSON_AcksAsPoison(t *testing.T) {
	t.Parallel()
	s := &fakeScanner{}
	store := &fakeStore{}
	pub := &fakePublisher{}
	h := newHandler(t, s, store, pub)

	msg := &fakeDelivery{body: []byte("not-json")}
	if err := h.Handle(context.Background(), msg); err != nil {
		t.Fatalf("expected nil err for poison, got %v", err)
	}
	if msg.acked.Load() != 1 {
		t.Fatal("poison should be acked, not redelivered")
	}
	if s.calls.Load() != 0 {
		t.Fatal("scanner should not be called for bad json")
	}
}

func TestHandle_IncompletePayload_AcksAsPoison(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  worker.Request
	}{
		{"zero tenant", worker.Request{MessageID: uuid.New(), Key: "k"}},
		{"zero message", worker.Request{TenantID: uuid.New(), Key: "k"}},
		{"empty key", worker.Request{TenantID: uuid.New(), MessageID: uuid.New()}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body, _ := json.Marshal(tc.req)
			s := &fakeScanner{}
			pub := &fakePublisher{}
			h := newHandler(t, s, &fakeStore{}, pub)
			msg := &fakeDelivery{body: body}
			if err := h.Handle(context.Background(), msg); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if msg.acked.Load() != 1 {
				t.Fatalf("expected ack for poison; got %d", msg.acked.Load())
			}
			if s.calls.Load() != 0 {
				t.Fatal("scanner should not be called")
			}
			if len(pub.bodies) != 0 {
				t.Fatal("publisher should not see poison")
			}
		})
	}
}

func TestHandle_ScannerError_NoAck_NoPublish(t *testing.T) {
	t.Parallel()
	s := &fakeScanner{err: errors.New("clamav unreachable")}
	store := &fakeStore{}
	pub := &fakePublisher{}
	h := newHandler(t, s, store, pub)

	msg := &fakeDelivery{body: makeReq(t, "media/x")}
	err := h.Handle(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error so broker redelivers")
	}
	if msg.acked.Load() != 0 {
		t.Fatal("must not ack on scanner failure")
	}
	if len(store.received) != 0 || len(pub.bodies) != 0 {
		t.Fatal("must not persist or publish on scanner failure")
	}
}

func TestHandle_PersistError_NoAck(t *testing.T) {
	t.Parallel()
	s := &fakeScanner{result: scanner.ScanResult{Status: scanner.StatusClean, EngineID: "x"}}
	store := &fakeStore{err: errors.New("db down")}
	pub := &fakePublisher{}
	h := newHandler(t, s, store, pub)

	msg := &fakeDelivery{body: makeReq(t, "media/x")}
	if err := h.Handle(context.Background(), msg); err == nil {
		t.Fatal("expected error so broker redelivers")
	}
	if msg.acked.Load() != 0 {
		t.Fatal("must not ack on persist failure")
	}
	if len(pub.bodies) != 0 {
		t.Fatal("must not publish before persist confirms")
	}
}

func TestHandle_RedeliveryAgainstFinalised_AcksWithoutRepublish(t *testing.T) {
	t.Parallel()
	s := &fakeScanner{result: scanner.ScanResult{Status: scanner.StatusClean, EngineID: "x"}}
	store := &fakeStore{err: scanner.ErrAlreadyFinalised}
	pub := &fakePublisher{}
	h := newHandler(t, s, store, pub)

	msg := &fakeDelivery{body: makeReq(t, "media/x")}
	if err := h.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if msg.acked.Load() != 1 {
		t.Fatal("redelivery against finalised row must be acked")
	}
	if len(pub.bodies) != 0 {
		t.Fatal("must not republish; downstream already saw the verdict")
	}
}

func TestHandle_MissingRow_AcksAsPoison(t *testing.T) {
	t.Parallel()
	s := &fakeScanner{result: scanner.ScanResult{Status: scanner.StatusClean, EngineID: "x"}}
	store := &fakeStore{err: scanner.ErrNotFound}
	pub := &fakePublisher{}
	h := newHandler(t, s, store, pub)

	msg := &fakeDelivery{body: makeReq(t, "media/x")}
	if err := h.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if msg.acked.Load() != 1 {
		t.Fatal("missing row must be acked (poison)")
	}
}

func TestHandle_PublishError_NoAck(t *testing.T) {
	t.Parallel()
	s := &fakeScanner{result: scanner.ScanResult{Status: scanner.StatusClean, EngineID: "x"}}
	store := &fakeStore{}
	pub := &fakePublisher{err: errors.New("nats publish failed")}
	h := newHandler(t, s, store, pub)

	msg := &fakeDelivery{body: makeReq(t, "media/x")}
	if err := h.Handle(context.Background(), msg); err == nil {
		t.Fatal("expected error so broker redelivers")
	}
	if msg.acked.Load() != 0 {
		t.Fatal("must not ack when publish fails")
	}
	// persistence happened — that's fine; the redelivery will hit
	// ErrAlreadyFinalised next time and ack.
	if len(store.received) != 1 {
		t.Fatalf("expected one persist, got %d", len(store.received))
	}
}

func TestHandle_AckFailure_Surfaces(t *testing.T) {
	t.Parallel()
	s := &fakeScanner{result: scanner.ScanResult{Status: scanner.StatusClean, EngineID: "x"}}
	store := &fakeStore{}
	pub := &fakePublisher{}
	h := newHandler(t, s, store, pub)

	msg := &fakeDelivery{body: makeReq(t, "media/x"), ackErr: errors.New("nack timeout")}
	if err := h.Handle(context.Background(), msg); err == nil {
		t.Fatal("expected ack failure to surface")
	}
}
