package dunning_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	billingdunning "github.com/pericles-luz/crm/internal/billing/dunning"
	dunningworker "github.com/pericles-luz/crm/internal/worker/dunning"
)

// fakeLister replays a fixed slice of candidates.
type fakeLister struct {
	rows []dunningworker.Candidate
	err  error
}

func (f *fakeLister) ListCandidates(_ context.Context, _ time.Time, _ int) ([]dunningworker.Candidate, error) {
	if f.err != nil {
		return nil, f.err
	}
	// Return a defensive copy so the worker's mutations don't bleed
	// into subsequent assertions on the test's source slice.
	out := make([]dunningworker.Candidate, len(f.rows))
	copy(out, f.rows)
	return out, nil
}

// recordingSaver counts saves and stores the last persisted state.
type recordingSaver struct {
	mu     sync.Mutex
	count  int
	last   *billingdunning.DunningState
	actor  uuid.UUID
	failOn map[uuid.UUID]error
}

func (r *recordingSaver) Save(_ context.Context, d *billingdunning.DunningState, actor uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.failOn[d.SubscriptionID()]; ok {
		return e
	}
	r.count++
	r.last = d
	r.actor = actor
	return nil
}

// fakeCourtesy returns canned overrides per tenant.
type fakeCourtesy struct {
	byTenant map[uuid.UUID]billingdunning.Override
}

func (f *fakeCourtesy) ActiveFor(_ context.Context, tenant uuid.UUID, _ time.Time) (billingdunning.Override, error) {
	if o, ok := f.byTenant[tenant]; ok {
		return o, nil
	}
	return billingdunning.Override{}, billingdunning.ErrNoActiveOverride
}

func mustDunning(t *testing.T, tenant, sub uuid.UUID, state billingdunning.State, enteredAt time.Time) *billingdunning.DunningState {
	t.Helper()
	row := billingdunning.HydrateDunningState(
		uuid.New(), tenant, sub, state, enteredAt, uuid.Nil, nil, "",
	)
	return row
}

func newWorker(t *testing.T, cfg dunningworker.Config) (*dunningworker.Worker, *dunningworker.Metrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := dunningworker.NewMetrics(reg)
	cfg.Metrics = m
	if cfg.ActorID == uuid.Nil {
		cfg.ActorID = uuid.New()
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	if cfg.Courtesy == nil {
		cfg.Courtesy = dunningworker.NoCourtesyOverride{}
	}
	w, err := dunningworker.New(cfg)
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	return w, m, reg
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestWorker_NewRequiresFields(t *testing.T) {
	saver := &recordingSaver{}
	lister := &fakeLister{}
	courtesy := dunningworker.NoCourtesyOverride{}
	actor := uuid.New()

	cases := []struct {
		name string
		cfg  dunningworker.Config
		want string
	}{
		{"no candidates", dunningworker.Config{Saver: saver, Courtesy: courtesy, ActorID: actor}, "CandidatesLister"},
		{"no saver", dunningworker.Config{Candidates: lister, Courtesy: courtesy, ActorID: actor}, "Saver"},
		{"no courtesy", dunningworker.Config{Candidates: lister, Saver: saver, ActorID: actor}, "CourtesyOverride"},
		{"no actor", dunningworker.Config{Candidates: lister, Saver: saver, Courtesy: courtesy}, "ActorID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := dunningworker.New(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestWorker_Tick_Escalates_AtThreshold(t *testing.T) {
	tenant := uuid.New()
	sub := uuid.New()
	plan := uuid.New()
	invoice := uuid.New()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	// 8 days past due → suspended_outbound (D+7 threshold).
	dueDate := now.Add(-8 * 24 * time.Hour)
	row := mustDunning(t, tenant, sub, billingdunning.StateCurrent, dueDate)
	cand := dunningworker.Candidate{
		Row:            row,
		SubscriptionID: sub,
		TenantID:       tenant,
		PlanID:         plan,
		Pending:        &dunningworker.PendingInvoice{ID: invoice, PeriodStart: dueDate},
	}
	saver := &recordingSaver{}
	w, m, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{rows: []dunningworker.Candidate{cand}},
		Saver:      saver,
		Clock:      func() time.Time { return now },
	})

	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if saver.count != 1 {
		t.Fatalf("save count = %d, want 1", saver.count)
	}
	if got := saver.last.State(); got != billingdunning.StateSuspendedOutbound {
		t.Errorf("state = %v, want suspended_outbound", got)
	}
	if got := testutil.ToFloat64(m.TransitionsTotal.WithLabelValues("current", "suspended_outbound")); got != 1 {
		t.Errorf("transitions(current→suspended_outbound) = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.StateGauge.WithLabelValues("suspended_outbound")); got != 1 {
		t.Errorf("gauge[suspended_outbound] = %v, want 1", got)
	}
}

func TestWorker_Tick_Escalates_31DaysFull(t *testing.T) {
	tenant, sub, plan, invoice := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	dueDate := now.Add(-31 * 24 * time.Hour)

	row := mustDunning(t, tenant, sub, billingdunning.StateSuspendedOutbound, dueDate.Add(24*time.Hour))
	cand := dunningworker.Candidate{
		Row:            row,
		SubscriptionID: sub,
		TenantID:       tenant,
		PlanID:         plan,
		Pending:        &dunningworker.PendingInvoice{ID: invoice, PeriodStart: dueDate},
	}
	saver := &recordingSaver{}
	w, _, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{rows: []dunningworker.Candidate{cand}},
		Saver:      saver,
		Clock:      func() time.Time { return now },
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := saver.last.State(); got != billingdunning.StateSuspendedFull {
		t.Errorf("state = %v, want suspended_full", got)
	}
}

func TestWorker_Tick_Idempotent_NoTransition(t *testing.T) {
	tenant, sub, plan, invoice := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	dueDate := now.Add(-8 * 24 * time.Hour)

	// Row is already at suspended_outbound; tick at the same boundary
	// must NOT call Save again.
	row := mustDunning(t, tenant, sub, billingdunning.StateSuspendedOutbound, dueDate.Add(8*24*time.Hour))
	cand := dunningworker.Candidate{
		Row:            row,
		SubscriptionID: sub,
		TenantID:       tenant,
		PlanID:         plan,
		Pending:        &dunningworker.PendingInvoice{ID: invoice, PeriodStart: dueDate},
	}
	saver := &recordingSaver{}
	w, _, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{rows: []dunningworker.Candidate{cand}},
		Saver:      saver,
		Clock:      func() time.Time { return now },
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if saver.count != 0 {
		t.Fatalf("idempotent tick saved %d times, want 0", saver.count)
	}
}

func TestWorker_Tick_NoPending_Downgrades(t *testing.T) {
	tenant, sub, plan := uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	row := mustDunning(t, tenant, sub, billingdunning.StateSuspendedOutbound, now.Add(-7*24*time.Hour))
	cand := dunningworker.Candidate{
		Row:            row,
		SubscriptionID: sub,
		TenantID:       tenant,
		PlanID:         plan,
		Pending:        nil, // payment confirmed elsewhere; no past-due
	}
	saver := &recordingSaver{}
	w, m, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{rows: []dunningworker.Candidate{cand}},
		Saver:      saver,
		Clock:      func() time.Time { return now },
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := saver.last.State(); got != billingdunning.StateCurrent {
		t.Errorf("state = %v, want current", got)
	}
	if got := testutil.ToFloat64(m.TransitionsTotal.WithLabelValues("suspended_outbound", "current")); got != 1 {
		t.Errorf("transitions(suspended_outbound→current) = %v, want 1", got)
	}
}

func TestWorker_Tick_AppliesCourtesyOverride(t *testing.T) {
	tenant, sub, plan, invoice := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	dueDate := now.Add(-30 * 24 * time.Hour)

	row := mustDunning(t, tenant, sub, billingdunning.StateSuspendedOutbound, dueDate.Add(7*24*time.Hour))
	cand := dunningworker.Candidate{
		Row:            row,
		SubscriptionID: sub,
		TenantID:       tenant,
		PlanID:         plan,
		Pending:        &dunningworker.PendingInvoice{ID: invoice, PeriodStart: dueDate},
	}
	saver := &recordingSaver{}
	courtesy := &fakeCourtesy{byTenant: map[uuid.UUID]billingdunning.Override{
		tenant: {
			Until:  now.Add(30 * 24 * time.Hour),
			Reason: "Free month courtesy after billing incident.",
		},
	}}
	w, _, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{rows: []dunningworker.Candidate{cand}},
		Saver:      saver,
		Courtesy:   courtesy,
		Clock:      func() time.Time { return now },
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := saver.last.State(); got != billingdunning.StateCurrent {
		t.Errorf("state = %v, want current (override resets)", got)
	}
	if got := saver.last.OverrideUntil(); got == nil || !got.Equal(now.Add(30*24*time.Hour)) {
		t.Errorf("override = %v, want %v", got, now.Add(30*24*time.Hour))
	}
}

func TestWorker_Tick_RespectsActiveOverride(t *testing.T) {
	tenant, sub, plan, invoice := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	dueDate := now.Add(-40 * 24 * time.Hour)

	// Row already has the override cached; pending invoice is past-due
	// (would otherwise escalate). The active override must keep the
	// row at current with NO save.
	until := now.Add(30 * 24 * time.Hour)
	row := billingdunning.HydrateDunningState(
		uuid.New(), tenant, sub, billingdunning.StateCurrent, now,
		uuid.Nil, &until, "Free month courtesy after billing incident.",
	)
	cand := dunningworker.Candidate{
		Row:            row,
		SubscriptionID: sub,
		TenantID:       tenant,
		PlanID:         plan,
		Pending:        &dunningworker.PendingInvoice{ID: invoice, PeriodStart: dueDate},
	}
	saver := &recordingSaver{}
	courtesy := &fakeCourtesy{byTenant: map[uuid.UUID]billingdunning.Override{
		tenant: {Until: until, Reason: "Free month courtesy after billing incident."},
	}}
	w, _, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{rows: []dunningworker.Candidate{cand}},
		Saver:      saver,
		Courtesy:   courtesy,
		Clock:      func() time.Time { return now },
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if saver.count != 0 {
		t.Fatalf("saved %d times under active override, want 0", saver.count)
	}
}

func TestWorker_Tick_CancelledRowExcluded(t *testing.T) {
	// The lister contract excludes cancelled rows; assert the worker
	// does no harm if one slips through (defense-in-depth: Escalate is
	// a no-op for cancelled).
	tenant, sub, plan, invoice := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	row := mustDunning(t, tenant, sub, billingdunning.StateCancelled, now.Add(-365*24*time.Hour))
	cand := dunningworker.Candidate{
		Row:            row,
		SubscriptionID: sub,
		TenantID:       tenant,
		PlanID:         plan,
		Pending:        &dunningworker.PendingInvoice{ID: invoice, PeriodStart: now.Add(-100 * 24 * time.Hour)},
	}
	saver := &recordingSaver{}
	w, _, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{rows: []dunningworker.Candidate{cand}},
		Saver:      saver,
		Clock:      func() time.Time { return now },
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if saver.count != 0 {
		t.Fatalf("cancelled row touched %d times, want 0", saver.count)
	}
}

func TestWorker_OnPaymentConfirmed_Downgrades(t *testing.T) {
	tenant, sub := uuid.New(), uuid.New()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	row := mustDunning(t, tenant, sub, billingdunning.StateSuspendedFull, now.Add(-40*24*time.Hour))
	saver := &recordingSaver{}
	w, m, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{},
		Saver:      saver,
		Clock:      func() time.Time { return now },
	})
	loader := func(_ context.Context, id uuid.UUID) (*billingdunning.DunningState, error) {
		if id != sub {
			t.Fatalf("loader called with %v, want %v", id, sub)
		}
		return row, nil
	}
	if err := w.OnPaymentConfirmed(context.Background(), loader, sub); err != nil {
		t.Fatalf("OnPaymentConfirmed: %v", err)
	}
	if got := saver.last.State(); got != billingdunning.StateCurrent {
		t.Errorf("state = %v, want current", got)
	}
	if got := testutil.ToFloat64(m.TransitionsTotal.WithLabelValues("suspended_full", "current")); got != 1 {
		t.Errorf("transitions(suspended_full→current) = %v, want 1", got)
	}
}

func TestWorker_OnPaymentConfirmed_ZeroSubscription(t *testing.T) {
	saver := &recordingSaver{}
	w, _, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{},
		Saver:      saver,
	})
	loader := func(context.Context, uuid.UUID) (*billingdunning.DunningState, error) {
		return nil, nil
	}
	if err := w.OnPaymentConfirmed(context.Background(), loader, uuid.Nil); !errors.Is(err, billingdunning.ErrZeroSubscription) {
		t.Fatalf("err = %v, want ErrZeroSubscription", err)
	}
}

func TestWorker_OnPaymentConfirmed_LoaderError(t *testing.T) {
	tenantSub := uuid.New()
	saver := &recordingSaver{}
	w, _, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{},
		Saver:      saver,
	})
	wantErr := errors.New("boom")
	loader := func(context.Context, uuid.UUID) (*billingdunning.DunningState, error) {
		return nil, wantErr
	}
	if err := w.OnPaymentConfirmed(context.Background(), loader, tenantSub); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if saver.count != 0 {
		t.Errorf("save count = %d, want 0 on loader err", saver.count)
	}
}

func TestWorker_OnPaymentConfirmed_IdempotentAtCurrent(t *testing.T) {
	tenant, sub := uuid.New(), uuid.New()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	row := mustDunning(t, tenant, sub, billingdunning.StateCurrent, now)
	saver := &recordingSaver{}
	w, m, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{},
		Saver:      saver,
		Clock:      func() time.Time { return now },
	})
	loader := func(context.Context, uuid.UUID) (*billingdunning.DunningState, error) { return row, nil }
	if err := w.OnPaymentConfirmed(context.Background(), loader, sub); err != nil {
		t.Fatalf("OnPaymentConfirmed: %v", err)
	}
	if saver.count != 1 {
		t.Errorf("save count = %d, want 1 (idempotent save)", saver.count)
	}
	// No transition counter increment because from == current.
	if got := testutil.ToFloat64(m.TransitionsTotal.WithLabelValues("current", "current")); got != 0 {
		t.Errorf("transitions(current→current) = %v, want 0", got)
	}
}

func TestWorker_OnPaymentConfirmed_RejectsCancelled(t *testing.T) {
	tenant, sub := uuid.New(), uuid.New()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	row := mustDunning(t, tenant, sub, billingdunning.StateCancelled, now.Add(-90*24*time.Hour))
	saver := &recordingSaver{}
	w, _, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{},
		Saver:      saver,
		Clock:      func() time.Time { return now },
	})
	loader := func(context.Context, uuid.UUID) (*billingdunning.DunningState, error) { return row, nil }
	if err := w.OnPaymentConfirmed(context.Background(), loader, sub); !errors.Is(err, billingdunning.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
}

func TestWorker_NoCourtesyOverride_ReturnsErrNoActiveOverride(t *testing.T) {
	_, err := dunningworker.NoCourtesyOverride{}.ActiveFor(context.Background(), uuid.New(), time.Now())
	if !errors.Is(err, billingdunning.ErrNoActiveOverride) {
		t.Fatalf("err = %v, want ErrNoActiveOverride", err)
	}
}

func TestWorker_Tick_ListerError(t *testing.T) {
	w, _, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{err: errors.New("db down")},
		Saver:      &recordingSaver{},
	})
	if err := w.Tick(context.Background()); err == nil {
		t.Fatal("Tick returned nil on lister error")
	}
}

func TestWorker_Run_CancelsCleanly(t *testing.T) {
	w, _, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{},
		Saver:      &recordingSaver{},
		TickEvery:  10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	// Let one initial tick run, then cancel.
	time.Sleep(15 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestWorker_Tick_SaverErrorOnEscalate(t *testing.T) {
	tenant, sub, plan, invoice := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	dueDate := now.Add(-8 * 24 * time.Hour)

	row := mustDunning(t, tenant, sub, billingdunning.StateCurrent, dueDate)
	cand := dunningworker.Candidate{
		Row: row, SubscriptionID: sub, TenantID: tenant, PlanID: plan,
		Pending: &dunningworker.PendingInvoice{ID: invoice, PeriodStart: dueDate},
	}
	saver := &recordingSaver{failOn: map[uuid.UUID]error{sub: errors.New("db conflict")}}
	w, m, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{rows: []dunningworker.Candidate{cand}},
		Saver:      saver,
		Clock:      func() time.Time { return now },
	})
	// The tick logs the error but does not return it (errors are
	// non-fatal at row scope; the next tick retries).
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	// No transition was counted because the save failed.
	if got := testutil.ToFloat64(m.TransitionsTotal.WithLabelValues("current", "suspended_outbound")); got != 0 {
		t.Errorf("transitions(current→suspended_outbound) = %v, want 0 on save failure", got)
	}
}

func TestWorker_Tick_SaverErrorOnDowngrade(t *testing.T) {
	tenant, sub, plan := uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	row := mustDunning(t, tenant, sub, billingdunning.StateSuspendedFull, now.Add(-30*24*time.Hour))
	cand := dunningworker.Candidate{
		Row: row, SubscriptionID: sub, TenantID: tenant, PlanID: plan, Pending: nil,
	}
	saver := &recordingSaver{failOn: map[uuid.UUID]error{sub: errors.New("write failed")}}
	w, m, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{rows: []dunningworker.Candidate{cand}},
		Saver:      saver,
		Clock:      func() time.Time { return now },
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := testutil.ToFloat64(m.TransitionsTotal.WithLabelValues("suspended_full", "current")); got != 0 {
		t.Errorf("transitions counted despite save failure: %v", got)
	}
}

func TestWorker_Tick_SaverErrorOnOverrideApply(t *testing.T) {
	tenant, sub, plan, invoice := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	dueDate := now.Add(-20 * 24 * time.Hour)
	row := mustDunning(t, tenant, sub, billingdunning.StateSuspendedOutbound, dueDate.Add(7*24*time.Hour))
	cand := dunningworker.Candidate{
		Row: row, SubscriptionID: sub, TenantID: tenant, PlanID: plan,
		Pending: &dunningworker.PendingInvoice{ID: invoice, PeriodStart: dueDate},
	}
	courtesy := &fakeCourtesy{byTenant: map[uuid.UUID]billingdunning.Override{
		tenant: {Until: now.Add(30 * 24 * time.Hour), Reason: "Free month courtesy after billing incident."},
	}}
	saver := &recordingSaver{failOn: map[uuid.UUID]error{sub: errors.New("write conflict")}}
	w, _, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{rows: []dunningworker.Candidate{cand}},
		Saver:      saver,
		Courtesy:   courtesy,
		Clock:      func() time.Time { return now },
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

// errCourtesy is a courtesy port that always fails. The worker must log
// and continue (escalation falls through with no override).
type errCourtesy struct{ err error }

func (e *errCourtesy) ActiveFor(context.Context, uuid.UUID, time.Time) (billingdunning.Override, error) {
	return billingdunning.Override{}, e.err
}

func TestWorker_Tick_CourtesyErrorContinues(t *testing.T) {
	tenant, sub, plan, invoice := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	dueDate := now.Add(-8 * 24 * time.Hour)
	row := mustDunning(t, tenant, sub, billingdunning.StateCurrent, dueDate)
	cand := dunningworker.Candidate{
		Row: row, SubscriptionID: sub, TenantID: tenant, PlanID: plan,
		Pending: &dunningworker.PendingInvoice{ID: invoice, PeriodStart: dueDate},
	}
	saver := &recordingSaver{}
	w, _, _ := newWorker(t, dunningworker.Config{
		Candidates: &fakeLister{rows: []dunningworker.Candidate{cand}},
		Saver:      saver,
		Courtesy:   &errCourtesy{err: errors.New("courtesy db down")},
		Clock:      func() time.Time { return now },
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	// Even with courtesy lookup failing, the row escalated because
	// override is treated as nil on error.
	if got := saver.last.State(); got != billingdunning.StateSuspendedOutbound {
		t.Errorf("state = %v, want suspended_outbound", got)
	}
}

func TestWorker_NilMetrics_NoPanic(t *testing.T) {
	tenant, sub, plan, invoice := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	dueDate := now.Add(-8 * 24 * time.Hour)
	row := mustDunning(t, tenant, sub, billingdunning.StateCurrent, dueDate)
	cand := dunningworker.Candidate{
		Row: row, SubscriptionID: sub, TenantID: tenant, PlanID: plan,
		Pending: &dunningworker.PendingInvoice{ID: invoice, PeriodStart: dueDate},
	}
	saver := &recordingSaver{}
	w, err := dunningworker.New(dunningworker.Config{
		Candidates: &fakeLister{rows: []dunningworker.Candidate{cand}},
		Saver:      saver,
		Courtesy:   dunningworker.NoCourtesyOverride{},
		ActorID:    uuid.New(),
		Logger:     slog.New(slog.NewTextHandler(discard{}, nil)),
		Clock:      func() time.Time { return now },
		// Metrics intentionally nil to exercise the nil-receiver branches.
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := saver.last.State(); got != billingdunning.StateSuspendedOutbound {
		t.Errorf("state = %v, want suspended_outbound", got)
	}
}

func TestWorker_Tick_RecordsLatency(t *testing.T) {
	tenant, sub, plan, invoice := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	dueDate := now.Add(-2 * 24 * time.Hour)
	row := mustDunning(t, tenant, sub, billingdunning.StateCurrent, now.Add(-time.Hour))
	cand := dunningworker.Candidate{
		Row:            row,
		SubscriptionID: sub,
		TenantID:       tenant,
		PlanID:         plan,
		Pending:        &dunningworker.PendingInvoice{ID: invoice, PeriodStart: dueDate},
	}
	saver := &recordingSaver{}
	reg := prometheus.NewRegistry()
	m := dunningworker.NewMetrics(reg)
	w, err := dunningworker.New(dunningworker.Config{
		Candidates: &fakeLister{rows: []dunningworker.Candidate{cand}},
		Saver:      saver,
		Courtesy:   dunningworker.NoCourtesyOverride{},
		Metrics:    m,
		ActorID:    uuid.New(),
		Clock:      func() time.Time { return now },
		Logger:     slog.New(slog.NewTextHandler(discard{}, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := testutil.CollectAndCount(m.TickLatency); got != 1 {
		t.Errorf("latency observations = %d, want 1", got)
	}
}
