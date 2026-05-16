package billing_test

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

	billingdomain "github.com/pericles-luz/crm/internal/billing"
	billingworker "github.com/pericles-luz/crm/internal/worker/billing"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeDue struct {
	rows []billingworker.DueSubscription
	err  error
}

func (f *fakeDue) ListDueSubscriptions(_ context.Context, _ time.Time, _ int) ([]billingworker.DueSubscription, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]billingworker.DueSubscription, len(f.rows))
	copy(out, f.rows)
	return out, nil
}

type fakeRenewer struct {
	calls       int
	resultBySub map[uuid.UUID]billingworker.RenewResult
	errBySub    map[uuid.UUID]error
}

func (f *fakeRenewer) RenewSubscription(
	_ context.Context,
	subID uuid.UUID,
	oldPeriodEnd time.Time,
	_ int,
	_ uuid.UUID,
	_ time.Time,
) (billingworker.RenewResult, error) {
	f.calls++
	if err, ok := f.errBySub[subID]; ok {
		return billingworker.RenewResult{}, err
	}
	if res, ok := f.resultBySub[subID]; ok {
		return res, nil
	}
	// Default success: build a deterministic result.
	periodStart := oldPeriodEnd
	periodEnd := oldPeriodEnd.AddDate(0, 1, 0)
	tenantID := uuid.New()
	planID := uuid.New()
	sub := billingdomain.HydrateSubscription(
		subID, tenantID, planID,
		billingdomain.SubscriptionStatusActive,
		periodStart, periodEnd,
		periodStart, periodStart,
	)
	inv, _ := billingdomain.NewInvoice(tenantID, subID, periodStart, periodEnd, 4990, oldPeriodEnd)
	return billingworker.RenewResult{
		Invoice:        inv,
		Subscription:   sub,
		NewPeriodStart: periodStart,
		NewPeriodEnd:   periodEnd,
	}, nil
}

type fakePublisher struct {
	mu       sync.Mutex
	calls    []publishCall
	errs     []error
	cursor   int
	failHook func(call int) error
}

type publishCall struct {
	Subject string
	MsgID   string
	Body    []byte
}

func (f *fakePublisher) Publish(_ context.Context, subject, msgID string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	bodyCopy := append([]byte(nil), body...)
	f.calls = append(f.calls, publishCall{Subject: subject, MsgID: msgID, Body: bodyCopy})
	if f.failHook != nil {
		return f.failHook(len(f.calls))
	}
	if f.cursor < len(f.errs) {
		err := f.errs[f.cursor]
		f.cursor++
		return err
	}
	return nil
}

func (f *fakePublisher) Calls() []publishCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]publishCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func baseConfig(t *testing.T) (billingworker.Config, *fakeDue, *fakeRenewer, *fakePublisher, *billingworker.Metrics) {
	t.Helper()
	due := &fakeDue{}
	rn := &fakeRenewer{
		resultBySub: map[uuid.UUID]billingworker.RenewResult{},
		errBySub:    map[uuid.UUID]error{},
	}
	pub := &fakePublisher{}
	reg := prometheus.NewRegistry()
	m := billingworker.NewMetrics(reg)
	cfg := billingworker.Config{
		Due:               due,
		Renewer:           rn,
		Publisher:         pub,
		Clock:             func() time.Time { return time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC) },
		Logger:            silentLogger(),
		Metrics:           m,
		ActorID:           uuid.New(),
		TickEvery:         time.Hour,
		BatchSize:         100,
		PublishMaxRetries: 3,
		PublishBaseDelay:  time.Millisecond,
		PublishMaxDelay:   2 * time.Millisecond,
	}
	return cfg, due, rn, pub, m
}

// ---------------------------------------------------------------------------
// New / Config validation
// ---------------------------------------------------------------------------

func TestNew_ValidationErrors(t *testing.T) {
	good, _, _, _, _ := baseConfig(t)

	cases := []struct {
		name    string
		mutate  func(*billingworker.Config)
		wantErr string
	}{
		{"missing Due", func(c *billingworker.Config) { c.Due = nil }, "DueSubscriptionsLister"},
		{"missing Renewer", func(c *billingworker.Config) { c.Renewer = nil }, "SubscriptionRenewer"},
		{"missing Publisher", func(c *billingworker.Config) { c.Publisher = nil }, "EventPublisher"},
		{"zero ActorID", func(c *billingworker.Config) { c.ActorID = uuid.Nil }, "ActorID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := good
			tc.mutate(&cfg)
			if _, err := billingworker.New(cfg); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("New() err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestNew_AppliesDefaults(t *testing.T) {
	cfg, _, _, _, _ := baseConfig(t)
	cfg.Clock = nil
	cfg.Logger = nil
	cfg.TickEvery = 0
	cfg.BatchSize = 0
	cfg.PublishMaxRetries = 0
	cfg.PublishBaseDelay = 0
	cfg.PublishMaxDelay = 0
	r, err := billingworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Errorf("Tick: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tick: golden path and error branches
// ---------------------------------------------------------------------------

func TestTick_SuccessPublishesAndIncrementsMetrics(t *testing.T) {
	cfg, due, _, pub, m := baseConfig(t)

	subID := uuid.New()
	tenantID := uuid.New()
	planID := uuid.New()
	oldPeriodEnd := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	due.rows = []billingworker.DueSubscription{
		{ID: subID, TenantID: tenantID, PlanID: planID, PlanPriceCents: 4990, CurrentPeriodEnd: oldPeriodEnd},
	}

	r, err := billingworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	calls := pub.Calls()
	if len(calls) != 1 {
		t.Fatalf("publish calls = %d, want 1", len(calls))
	}
	if calls[0].Subject != billingworker.SubjectSubscriptionRenewed {
		t.Errorf("subject = %q, want %q", calls[0].Subject, billingworker.SubjectSubscriptionRenewed)
	}
	wantPrefix := subID.String() + ":"
	if !strings.HasPrefix(calls[0].MsgID, wantPrefix) {
		t.Errorf("msgID = %q, want prefix %q", calls[0].MsgID, wantPrefix)
	}

	var ev billingworker.Event
	if err := json.Unmarshal(calls[0].Body, &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if ev.SubscriptionID != subID {
		t.Errorf("event.SubscriptionID = %v, want %v", ev.SubscriptionID, subID)
	}
	if ev.AmountCentsBRL != 4990 {
		t.Errorf("event.AmountCentsBRL = %d, want 4990", ev.AmountCentsBRL)
	}
	if !ev.PreviousPeriodEnd.Equal(oldPeriodEnd) {
		t.Errorf("event.PreviousPeriodEnd = %v, want %v", ev.PreviousPeriodEnd, oldPeriodEnd)
	}
	if !ev.NewPeriodStart.Equal(oldPeriodEnd) {
		t.Errorf("event.NewPeriodStart = %v, want %v (== oldPeriodEnd)", ev.NewPeriodStart, oldPeriodEnd)
	}

	if got := testutil.ToFloat64(m.Runs.WithLabelValues(billingworker.OutcomeSuccess)); got != 1 {
		t.Errorf("runs[success] = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.Invoices.WithLabelValues("pending")); got != 1 {
		t.Errorf("invoices[pending] = %v, want 1", got)
	}
}

func TestTick_SkippedAlreadyDone(t *testing.T) {
	cfg, due, rn, pub, m := baseConfig(t)

	subID := uuid.New()
	due.rows = []billingworker.DueSubscription{
		{ID: subID, TenantID: uuid.New(), PlanID: uuid.New(), PlanPriceCents: 4990, CurrentPeriodEnd: time.Now()},
	}
	rn.errBySub[subID] = billingdomain.ErrInvoiceAlreadyExists

	r, err := billingworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := len(pub.Calls()); got != 0 {
		t.Errorf("publish calls = %d, want 0 (skip should not publish)", got)
	}
	if got := testutil.ToFloat64(m.Runs.WithLabelValues(billingworker.OutcomeSkippedAlreadyDone)); got != 1 {
		t.Errorf("runs[skipped_already_done] = %v, want 1", got)
	}
}

func TestTick_RenewerErrorCountsAsError(t *testing.T) {
	cfg, due, rn, pub, m := baseConfig(t)

	subID := uuid.New()
	due.rows = []billingworker.DueSubscription{
		{ID: subID, TenantID: uuid.New(), PlanID: uuid.New(), PlanPriceCents: 4990, CurrentPeriodEnd: time.Now()},
	}
	rn.errBySub[subID] = errors.New("boom")

	r, err := billingworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := len(pub.Calls()); got != 0 {
		t.Errorf("publish calls = %d, want 0 (db error should not publish)", got)
	}
	if got := testutil.ToFloat64(m.Runs.WithLabelValues(billingworker.OutcomeError)); got != 1 {
		t.Errorf("runs[error] = %v, want 1", got)
	}
}

func TestTick_ListErrorReturnsAndDoesNotPublish(t *testing.T) {
	cfg, due, _, pub, _ := baseConfig(t)
	due.err = errors.New("db dead")

	r, err := billingworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = r.Tick(context.Background())
	if err == nil {
		t.Fatal("Tick: want error, got nil")
	}
	if !strings.Contains(err.Error(), "db dead") {
		t.Errorf("err = %v, want substring 'db dead'", err)
	}
	if got := len(pub.Calls()); got != 0 {
		t.Errorf("publish calls = %d, want 0", got)
	}
}

func TestTick_PublishBackoffEventuallySucceeds(t *testing.T) {
	cfg, due, _, pub, m := baseConfig(t)

	due.rows = []billingworker.DueSubscription{
		{ID: uuid.New(), TenantID: uuid.New(), PlanID: uuid.New(), PlanPriceCents: 4990, CurrentPeriodEnd: time.Now()},
	}
	pub.errs = []error{errors.New("blip-1"), errors.New("blip-2")}

	r, err := billingworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := len(pub.Calls()); got != 3 {
		t.Errorf("publish calls = %d, want 3 (2 fail + 1 success)", got)
	}
	if got := testutil.ToFloat64(m.Runs.WithLabelValues(billingworker.OutcomeSuccess)); got != 1 {
		t.Errorf("runs[success] = %v, want 1", got)
	}
}

func TestTick_PublishExhaustsRetries(t *testing.T) {
	cfg, due, _, pub, m := baseConfig(t)

	due.rows = []billingworker.DueSubscription{
		{ID: uuid.New(), TenantID: uuid.New(), PlanID: uuid.New(), PlanPriceCents: 4990, CurrentPeriodEnd: time.Now()},
	}
	pub.failHook = func(_ int) error { return errors.New("nats down") }

	r, err := billingworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := len(pub.Calls()); got != cfg.PublishMaxRetries {
		t.Errorf("publish calls = %d, want %d (max retries)", got, cfg.PublishMaxRetries)
	}
	if got := testutil.ToFloat64(m.Runs.WithLabelValues(billingworker.OutcomeError)); got != 1 {
		t.Errorf("runs[error] = %v, want 1", got)
	}
}

func TestTick_PublishCancelledByContext(t *testing.T) {
	cfg, due, _, pub, _ := baseConfig(t)
	cfg.PublishBaseDelay = 100 * time.Millisecond
	cfg.PublishMaxDelay = 100 * time.Millisecond
	cfg.PublishMaxRetries = 10

	due.rows = []billingworker.DueSubscription{
		{ID: uuid.New(), TenantID: uuid.New(), PlanID: uuid.New(), PlanPriceCents: 4990, CurrentPeriodEnd: time.Now()},
	}
	pub.failHook = func(_ int) error { return errors.New("transient") }

	r, err := billingworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if err := r.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("Tick took %v, expected fast cancellation", elapsed)
	}
	if len(pub.Calls()) >= cfg.PublishMaxRetries {
		t.Errorf("publish calls = %d, expected < %d (cancellation should short-circuit)", len(pub.Calls()), cfg.PublishMaxRetries)
	}
}

// ---------------------------------------------------------------------------
// Run lifecycle
// ---------------------------------------------------------------------------

func TestRun_CancellationExits(t *testing.T) {
	cfg, _, _, _, _ := baseConfig(t)
	cfg.TickEvery = 100 * time.Millisecond

	r, err := billingworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	time.Sleep(30 * time.Millisecond)
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

// ---------------------------------------------------------------------------
// Idempotency: simulating two runs that hit the same period
// ---------------------------------------------------------------------------

func TestTick_TwoRunsSamePeriodYieldOneSuccessOneSkip(t *testing.T) {
	cfg, due, rn, pub, m := baseConfig(t)

	subID := uuid.New()
	due.rows = []billingworker.DueSubscription{
		{ID: subID, TenantID: uuid.New(), PlanID: uuid.New(), PlanPriceCents: 4990, CurrentPeriodEnd: time.Now()},
	}

	r, err := billingworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("first Tick: %v", err)
	}

	rn.errBySub[subID] = billingdomain.ErrInvoiceAlreadyExists
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}

	if got := testutil.ToFloat64(m.Runs.WithLabelValues(billingworker.OutcomeSuccess)); got != 1 {
		t.Errorf("runs[success] = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.Runs.WithLabelValues(billingworker.OutcomeSkippedAlreadyDone)); got != 1 {
		t.Errorf("runs[skipped_already_done] = %v, want 1", got)
	}
	if got := len(pub.Calls()); got != 1 {
		t.Errorf("publish calls = %d, want 1 (second tick must not publish)", got)
	}
}

// ---------------------------------------------------------------------------
// Metrics: nil receiver, NewMetrics(nil) registerer
// ---------------------------------------------------------------------------

func TestMetrics_NewWithNilRegistererAndUsage(t *testing.T) {
	m := billingworker.NewMetrics(nil)
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}
	// Counters increment regardless of registration.
	m.Runs.WithLabelValues(billingworker.OutcomeSuccess).Inc()
	m.Invoices.WithLabelValues("pending").Inc()
	if got := testutil.ToFloat64(m.Runs.WithLabelValues(billingworker.OutcomeSuccess)); got != 1 {
		t.Errorf("runs = %v, want 1", got)
	}
}

func TestRenewer_NilMetricsIsSafe(t *testing.T) {
	cfg, due, _, _, _ := baseConfig(t)
	cfg.Metrics = nil
	due.rows = []billingworker.DueSubscription{
		{ID: uuid.New(), TenantID: uuid.New(), PlanID: uuid.New(), PlanPriceCents: 4990, CurrentPeriodEnd: time.Now()},
	}
	r, err := billingworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Errorf("Tick with nil metrics: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Several due subscriptions in one tick
// ---------------------------------------------------------------------------

func TestTick_MultipleDueSubscriptions(t *testing.T) {
	cfg, due, _, pub, m := baseConfig(t)

	for i := 0; i < 3; i++ {
		due.rows = append(due.rows, billingworker.DueSubscription{
			ID:               uuid.New(),
			TenantID:         uuid.New(),
			PlanID:           uuid.New(),
			PlanPriceCents:   1000 * (i + 1),
			CurrentPeriodEnd: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
	}

	r, err := billingworker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := len(pub.Calls()); got != 3 {
		t.Errorf("publish calls = %d, want 3", got)
	}
	if got := testutil.ToFloat64(m.Runs.WithLabelValues(billingworker.OutcomeSuccess)); got != 3 {
		t.Errorf("runs[success] = %v, want 3", got)
	}
}

// Test guarantee: prometheus.Registry import is still used directly.
var _ = prometheus.NewRegistry
