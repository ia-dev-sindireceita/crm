package wallet_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pericles-luz/crm/internal/billing"
	walletworker "github.com/pericles-luz/crm/internal/worker/wallet"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeDelivery struct {
	data     []byte
	msgID    string
	ackCalls int
	nakCalls int
	lastNak  time.Duration
	ackErr   error
	nakErr   error
	mu       sync.Mutex
}

func (f *fakeDelivery) Data() []byte  { return f.data }
func (f *fakeDelivery) MsgID() string { return f.msgID }
func (f *fakeDelivery) Ack(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ackCalls++
	return f.ackErr
}
func (f *fakeDelivery) Nak(_ context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nakCalls++
	f.lastNak = d
	return f.nakErr
}

func (f *fakeDelivery) Acks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ackCalls
}

func (f *fakeDelivery) Naks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nakCalls
}

type fakePlans struct {
	plan billing.Plan
	err  error
	hits int
	mu   sync.Mutex
}

func (f *fakePlans) GetPlanByID(_ context.Context, id uuid.UUID) (billing.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	if f.err != nil {
		return billing.Plan{}, f.err
	}
	p := f.plan
	p.ID = id
	return p, nil
}

func (f *fakePlans) Hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

type fakeAllocator struct {
	mu sync.Mutex
	// idempotency: per idempotencyKey we record one "allocated" the
	// first time and "skipped_duplicate" thereafter.
	seenKeys map[string]bool
	calls    []allocCall
	// stubErr is returned once if set, then cleared.
	stubErr error
}

type allocCall struct {
	TenantID       uuid.UUID
	PeriodStart    time.Time
	Amount         int64
	IdempotencyKey string
}

func newFakeAllocator() *fakeAllocator { return &fakeAllocator{seenKeys: map[string]bool{}} }

func (f *fakeAllocator) AllocateMonthlyQuota(
	_ context.Context,
	tenantID uuid.UUID,
	periodStart time.Time,
	amount int64,
	idempotencyKey string,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stubErr != nil {
		err := f.stubErr
		f.stubErr = nil
		return false, err
	}
	f.calls = append(f.calls, allocCall{
		TenantID:       tenantID,
		PeriodStart:    periodStart,
		Amount:         amount,
		IdempotencyKey: idempotencyKey,
	})
	if f.seenKeys[idempotencyKey] {
		return false, nil
	}
	f.seenKeys[idempotencyKey] = true
	return true, nil
}

func (f *fakeAllocator) Calls() []allocCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]allocCall, len(f.calls))
	copy(out, f.calls)
	return out
}

type fakeSubscriber struct {
	deliveries chan walletworker.Delivery
	errs       chan error
	subErr     error
	subscribed bool
	mu         sync.Mutex
}

func newFakeSubscriber() *fakeSubscriber {
	return &fakeSubscriber{
		deliveries: make(chan walletworker.Delivery, 8),
		errs:       make(chan error, 1),
	}
}

func (f *fakeSubscriber) Subscribe(_ context.Context) (<-chan walletworker.Delivery, <-chan error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subErr != nil {
		return nil, nil, f.subErr
	}
	f.subscribed = true
	return f.deliveries, f.errs, nil
}

func silentLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func baseConfig(t *testing.T) (walletworker.Config, *fakeSubscriber, *fakePlans, *fakeAllocator, *walletworker.Metrics) {
	t.Helper()
	sub := newFakeSubscriber()
	plans := &fakePlans{plan: billing.Plan{Slug: "pro", MonthlyTokenQuota: 1_000_000}}
	alloc := newFakeAllocator()
	reg := prometheus.NewRegistry()
	m := walletworker.NewMetrics(reg)
	cfg := walletworker.Config{
		Subscriber: sub,
		Plans:      plans,
		Allocator:  alloc,
		Clock:      func() time.Time { return time.Date(2026, 5, 17, 0, 0, 30, 0, time.UTC) },
		Logger:     silentLogger(),
		Metrics:    m,
		NakDelay:   time.Millisecond,
	}
	return cfg, sub, plans, alloc, m
}

func buildEventBody(t *testing.T, ev walletworker.Event) []byte {
	t.Helper()
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return body
}

// ---------------------------------------------------------------------------
// New / Config validation
// ---------------------------------------------------------------------------

func TestNew_ValidationErrors(t *testing.T) {
	good, _, _, _, _ := baseConfig(t)

	cases := []struct {
		name    string
		mutate  func(*walletworker.Config)
		wantErr string
	}{
		{"missing Subscriber", func(c *walletworker.Config) { c.Subscriber = nil }, "EventSubscriber"},
		{"missing Plans", func(c *walletworker.Config) { c.Plans = nil }, "PlanReader"},
		{"missing Allocator", func(c *walletworker.Config) { c.Allocator = nil }, "MonthlyAllocator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := good
			tc.mutate(&cfg)
			_, err := walletworker.New(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("New() err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestNew_AppliesDefaults(t *testing.T) {
	cfg, _, _, _, _ := baseConfig(t)
	cfg.Clock = nil
	cfg.Logger = nil
	cfg.NakDelay = 0
	a, err := walletworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Drive a no-op Handle to make sure defaults work end-to-end.
	d := &fakeDelivery{} // empty msgID → drops; no panics is the goal
	a.Handle(context.Background(), d)
}

// ---------------------------------------------------------------------------
// Handle: happy path, decode/plan/allocate errors, idempotency
// ---------------------------------------------------------------------------

func TestHandle_GoldenPath_Allocates(t *testing.T) {
	cfg, _, _, alloc, m := baseConfig(t)
	a, err := walletworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tenantID := uuid.New()
	subID := uuid.New()
	planID := uuid.New()
	periodStart := time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC) // 30s before fake "now"
	body := buildEventBody(t, walletworker.Event{
		SubscriptionID: subID,
		TenantID:       tenantID,
		PlanID:         planID,
		InvoiceID:      uuid.New(),
		NewPeriodStart: periodStart,
		NewPeriodEnd:   periodStart.AddDate(0, 1, 0),
		AmountCentsBRL: 4990,
		RenewedAt:      periodStart,
	})
	d := &fakeDelivery{
		data:  body,
		msgID: subID.String() + ":" + periodStart.Format(time.RFC3339Nano),
	}

	a.Handle(context.Background(), d)

	if d.Acks() != 1 {
		t.Errorf("Ack calls = %d, want 1", d.Acks())
	}
	if d.Naks() != 0 {
		t.Errorf("Nak calls = %d, want 0", d.Naks())
	}
	calls := alloc.Calls()
	if len(calls) != 1 {
		t.Fatalf("allocator calls = %d, want 1", len(calls))
	}
	if calls[0].TenantID != tenantID {
		t.Errorf("tenant = %v, want %v", calls[0].TenantID, tenantID)
	}
	if !calls[0].PeriodStart.Equal(periodStart) {
		t.Errorf("periodStart = %v, want %v", calls[0].PeriodStart, periodStart)
	}
	if calls[0].Amount != 1_000_000 {
		t.Errorf("amount = %d, want 1_000_000", calls[0].Amount)
	}
	if calls[0].IdempotencyKey != d.msgID {
		t.Errorf("idempotencyKey = %q, want %q", calls[0].IdempotencyKey, d.msgID)
	}
	if got := testutil.ToFloat64(m.Allocations.WithLabelValues(walletworker.OutcomeAllocated)); got != 1 {
		t.Errorf("allocations[allocated] = %v, want 1", got)
	}
}

func TestHandle_DuplicateDelivery_IdempotentSkip(t *testing.T) {
	cfg, _, _, alloc, m := baseConfig(t)
	a, err := walletworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tenantID := uuid.New()
	subID := uuid.New()
	periodStart := time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)
	body := buildEventBody(t, walletworker.Event{
		SubscriptionID: subID,
		TenantID:       tenantID,
		PlanID:         uuid.New(),
		NewPeriodStart: periodStart,
	})
	msgID := subID.String() + ":" + periodStart.Format(time.RFC3339Nano)

	// First delivery — allocates.
	d1 := &fakeDelivery{data: body, msgID: msgID}
	a.Handle(context.Background(), d1)
	// Redelivery (NATS dedupe might not catch every case; allocator
	// idempotency is the cinturão de segurança).
	d2 := &fakeDelivery{data: body, msgID: msgID}
	a.Handle(context.Background(), d2)

	if got := len(alloc.Calls()); got != 2 {
		t.Errorf("allocator calls = %d, want 2 (fake records all calls)", got)
	}
	allocated := testutil.ToFloat64(m.Allocations.WithLabelValues(walletworker.OutcomeAllocated))
	skipped := testutil.ToFloat64(m.Allocations.WithLabelValues(walletworker.OutcomeSkippedDuplicate))
	if allocated != 1 || skipped != 1 {
		t.Errorf("allocations: allocated=%v skipped=%v, want 1/1", allocated, skipped)
	}
	if d1.Acks() != 1 || d2.Acks() != 1 {
		t.Errorf("Acks: d1=%d d2=%d, both want 1", d1.Acks(), d2.Acks())
	}
}

func TestHandle_DecodeError_AcksAndCounts(t *testing.T) {
	cfg, _, plans, alloc, m := baseConfig(t)
	a, err := walletworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	d := &fakeDelivery{
		data:  []byte("{not valid json"),
		msgID: "anything",
	}
	a.Handle(context.Background(), d)

	if d.Acks() != 1 {
		t.Errorf("Ack calls = %d, want 1 (poison-pill drop)", d.Acks())
	}
	if d.Naks() != 0 {
		t.Errorf("Nak calls = %d, want 0", d.Naks())
	}
	if plans.Hits() != 0 {
		t.Errorf("plan lookups = %d, want 0", plans.Hits())
	}
	if len(alloc.Calls()) != 0 {
		t.Errorf("allocator calls = %d, want 0", len(alloc.Calls()))
	}
	if got := testutil.ToFloat64(m.Allocations.WithLabelValues(walletworker.OutcomeFailedDecode)); got != 1 {
		t.Errorf("allocations[failed_decode] = %v, want 1", got)
	}
}

func TestHandle_MissingMsgID_AcksAndCounts(t *testing.T) {
	cfg, _, plans, alloc, m := baseConfig(t)
	a, err := walletworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := buildEventBody(t, walletworker.Event{
		SubscriptionID: uuid.New(),
		TenantID:       uuid.New(),
		PlanID:         uuid.New(),
		NewPeriodStart: time.Now(),
	})
	d := &fakeDelivery{data: body, msgID: ""}
	a.Handle(context.Background(), d)

	if d.Acks() != 1 {
		t.Errorf("Ack calls = %d, want 1", d.Acks())
	}
	if plans.Hits() != 0 {
		t.Errorf("plan lookups = %d, want 0 (we drop before lookup)", plans.Hits())
	}
	if len(alloc.Calls()) != 0 {
		t.Errorf("allocator calls = %d, want 0", len(alloc.Calls()))
	}
	if got := testutil.ToFloat64(m.Allocations.WithLabelValues(walletworker.OutcomeMissingMsgID)); got != 1 {
		t.Errorf("allocations[missing_msg_id] = %v, want 1", got)
	}
}

func TestHandle_PlanLookupError_NakAndCounts(t *testing.T) {
	cfg, _, plans, alloc, m := baseConfig(t)
	plans.err = errors.New("db blip")
	a, err := walletworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := buildEventBody(t, walletworker.Event{
		SubscriptionID: uuid.New(),
		TenantID:       uuid.New(),
		PlanID:         uuid.New(),
		NewPeriodStart: time.Now(),
	})
	d := &fakeDelivery{data: body, msgID: "msg-1"}
	a.Handle(context.Background(), d)

	if d.Acks() != 0 {
		t.Errorf("Ack calls = %d, want 0", d.Acks())
	}
	if d.Naks() != 1 {
		t.Errorf("Nak calls = %d, want 1", d.Naks())
	}
	if d.lastNak != cfg.NakDelay {
		t.Errorf("Nak delay = %v, want %v", d.lastNak, cfg.NakDelay)
	}
	if len(alloc.Calls()) != 0 {
		t.Errorf("allocator calls = %d, want 0", len(alloc.Calls()))
	}
	if got := testutil.ToFloat64(m.Allocations.WithLabelValues(walletworker.OutcomeFailedPlan)); got != 1 {
		t.Errorf("allocations[failed_plan] = %v, want 1", got)
	}
}

func TestHandle_AllocateError_NakAndCounts(t *testing.T) {
	cfg, _, _, alloc, m := baseConfig(t)
	alloc.stubErr = errors.New("wallet missing")
	a, err := walletworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := buildEventBody(t, walletworker.Event{
		SubscriptionID: uuid.New(),
		TenantID:       uuid.New(),
		PlanID:         uuid.New(),
		NewPeriodStart: time.Now(),
	})
	d := &fakeDelivery{data: body, msgID: "msg-allocate-fail"}
	a.Handle(context.Background(), d)

	if d.Acks() != 0 {
		t.Errorf("Ack calls = %d, want 0", d.Acks())
	}
	if d.Naks() != 1 {
		t.Errorf("Nak calls = %d, want 1", d.Naks())
	}
	if got := testutil.ToFloat64(m.Allocations.WithLabelValues(walletworker.OutcomeFailedAllocate)); got != 1 {
		t.Errorf("allocations[failed_allocate] = %v, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Lag metric: confirm the histogram observes period-start vs clock delta
// ---------------------------------------------------------------------------

func TestHandle_RecordsLag(t *testing.T) {
	cfg, _, _, _, m := baseConfig(t)
	a, err := walletworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Fake clock is 2026-05-17 00:00:30; period start 25s earlier.
	periodStart := time.Date(2026, 5, 17, 0, 0, 5, 0, time.UTC)
	body := buildEventBody(t, walletworker.Event{
		SubscriptionID: uuid.New(),
		TenantID:       uuid.New(),
		PlanID:         uuid.New(),
		NewPeriodStart: periodStart,
	})
	d := &fakeDelivery{data: body, msgID: "msg-lag"}
	a.Handle(context.Background(), d)

	// We can not directly read histogram observations via testutil
	// without a registry round-trip; use CollectAndCount as a sanity
	// check that exactly one sample landed.
	if got := testutil.CollectAndCount(m.Lag); got != 1 {
		t.Errorf("lag histogram observations = %d, want 1", got)
	}
	if got := testutil.CollectAndCount(m.HandleDuration); got != 1 {
		t.Errorf("duration histogram observations = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Run lifecycle: cancellation + subscriber errors
// ---------------------------------------------------------------------------

func TestRun_SubscribeError(t *testing.T) {
	cfg, sub, _, _, _ := baseConfig(t)
	sub.subErr = errors.New("nats down")

	a, err := walletworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = a.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nats down") {
		t.Errorf("Run err = %v, want substring 'nats down'", err)
	}
}

func TestRun_CancellationExits(t *testing.T) {
	cfg, _, _, _, _ := baseConfig(t)
	a, err := walletworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRun_TerminalSubscriberError(t *testing.T) {
	cfg, sub, _, _, _ := baseConfig(t)
	a, err := walletworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	sub.errs <- errors.New("broker closed")
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "broker closed") {
			t.Errorf("Run err = %v, want substring 'broker closed'", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after terminal subscriber error")
	}
}

func TestRun_ChannelClosedExitsCleanly(t *testing.T) {
	cfg, sub, _, _, _ := baseConfig(t)
	a, err := walletworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	close(sub.deliveries)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after deliveries channel closed")
	}
}

func TestRun_HandlesDeliveriesFromChannel(t *testing.T) {
	cfg, sub, _, alloc, _ := baseConfig(t)
	a, err := walletworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()

	body := buildEventBody(t, walletworker.Event{
		SubscriptionID: uuid.New(),
		TenantID:       uuid.New(),
		PlanID:         uuid.New(),
		NewPeriodStart: time.Now(),
	})
	d := &fakeDelivery{data: body, msgID: "msg-run-1"}
	sub.deliveries <- d

	// Wait until the allocator records the call (with a small timeout)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(alloc.Calls()) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(alloc.Calls()) != 1 {
		t.Fatalf("allocator calls = %d, want 1", len(alloc.Calls()))
	}
	cancel()
	<-done
}

// ---------------------------------------------------------------------------
// Metrics nil-safety
// ---------------------------------------------------------------------------

func TestMetrics_NilSafe(t *testing.T) {
	cfg, _, _, _, _ := baseConfig(t)
	cfg.Metrics = nil
	a, err := walletworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := buildEventBody(t, walletworker.Event{
		SubscriptionID: uuid.New(),
		TenantID:       uuid.New(),
		PlanID:         uuid.New(),
		NewPeriodStart: time.Now(),
	})
	a.Handle(context.Background(), &fakeDelivery{data: body, msgID: "x"})
}

func TestMetrics_NewWithNilRegisterer(t *testing.T) {
	m := walletworker.NewMetrics(nil)
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}
	m.Allocations.WithLabelValues(walletworker.OutcomeAllocated).Inc()
	if got := testutil.ToFloat64(m.Allocations.WithLabelValues(walletworker.OutcomeAllocated)); got != 1 {
		t.Errorf("allocations = %v, want 1", got)
	}
}

// Test guarantee: prometheus.Registry import is still used directly.
var _ = prometheus.NewRegistry
