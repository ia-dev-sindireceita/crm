package worker_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pericles-luz/crm/internal/media/alert"
	"github.com/pericles-luz/crm/internal/media/scanner"
	"github.com/pericles-luz/crm/internal/media/worker"
)

// fakeAlerter is the in-process double for worker_test alerter coverage.
type fakeAlerter struct {
	mu     sync.Mutex
	events []alert.Event
	err    error
	calls  atomic.Int32
}

func (f *fakeAlerter) Notify(_ context.Context, e alert.Event) error {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return f.err
}

func newAlerterHandler(t *testing.T, a alert.Alerter, scanResult scanner.ScanResult) (*worker.Handler, *fakeDelivery, *fakeStore) {
	t.Helper()
	s := &fakeScanner{result: scanResult}
	store := &fakeStore{}
	pub := &fakePublisher{}
	h, err := worker.New(s, store, pub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	h.Alerter = a
	msg := &fakeDelivery{body: makeReq(t, "media/tenant/eicar.pdf")}
	return h, msg, store
}

func TestHandle_InfectedVerdict_CallsAlerter(t *testing.T) {
	t.Parallel()
	a := &fakeAlerter{}
	h, msg, _ := newAlerterHandler(t, a, scanner.ScanResult{
		Status:    scanner.StatusInfected,
		EngineID:  "clamav-1.4.2",
		Signature: "Win.Test.EICAR_HDB-1",
	})
	if err := h.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if a.calls.Load() != 1 {
		t.Fatalf("expected one alert.Notify call, got %d", a.calls.Load())
	}
	got := a.events[0]
	if got.EngineID != "clamav-1.4.2" {
		t.Errorf("EngineID = %q", got.EngineID)
	}
	if got.Signature != "Win.Test.EICAR_HDB-1" {
		t.Errorf("Signature = %q", got.Signature)
	}
	if got.TenantID.String() != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("TenantID propagation: %v", got.TenantID)
	}
	if got.Key != "media/tenant/eicar.pdf" {
		t.Errorf("Key = %q", got.Key)
	}
}

func TestHandle_CleanVerdict_DoesNotCallAlerter(t *testing.T) {
	t.Parallel()
	a := &fakeAlerter{}
	h, msg, _ := newAlerterHandler(t, a, scanner.ScanResult{Status: scanner.StatusClean, EngineID: "x"})
	if err := h.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if a.calls.Load() != 0 {
		t.Fatalf("expected no alert on clean verdict, got %d", a.calls.Load())
	}
}

func TestHandle_AlerterError_DoesNotBlockAck(t *testing.T) {
	t.Parallel()
	a := &fakeAlerter{err: errors.New("slack: 429 rate-limited")}
	h, msg, _ := newAlerterHandler(t, a, scanner.ScanResult{
		Status:   scanner.StatusInfected,
		EngineID: "clamav-x",
	})
	if err := h.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle must not fail on alerter error: %v", err)
	}
	if msg.acked.Load() != 1 {
		t.Fatal("delivery must ack even when Alerter.Notify fails")
	}
}

func TestHandle_AlerterNil_NoPanic(t *testing.T) {
	t.Parallel()
	h, msg, _ := newAlerterHandler(t, nil, scanner.ScanResult{Status: scanner.StatusInfected, EngineID: "x"})
	if err := h.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if msg.acked.Load() != 1 {
		t.Fatal("nil Alerter must not block ack")
	}
}
