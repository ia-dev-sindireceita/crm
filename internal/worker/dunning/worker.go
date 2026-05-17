package dunning

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	billingdunning "github.com/pericles-luz/crm/internal/billing/dunning"
)

// Config bundles Worker dependencies. The zero value is invalid; New()
// returns an error if a required field is missing and fills defaults
// for the rest.
type Config struct {
	// Candidates lists non-terminal dunning rows to evaluate this tick.
	Candidates CandidatesLister
	// Saver persists DunningState mutations.
	Saver Saver
	// Courtesy looks up the live free_subscription_period override for
	// a tenant; supply a no-op (NoCourtesyOverride) when masters cannot
	// grant overrides in the deployment.
	Courtesy billingdunning.CourtesyOverride
	// Policy resolves a planID to its policy thresholds. Defaults to
	// DefaultPolicyResolver (DefaultPolicy for every plan).
	Policy PolicyResolver
	// Clock returns "now". Defaults to time.Now().UTC().
	Clock func() time.Time
	// Logger is the structured logger. Defaults to slog.Default().
	Logger *slog.Logger
	// Metrics is the Prometheus surface. nil disables metrics (safe).
	Metrics *Metrics
	// ActorID is the audit actor recorded by WithMasterOps. The system
	// uses a fixed UUID for the dunning worker so audit consumers can
	// distinguish dunning writes from operator writes.
	ActorID uuid.UUID
	// TickEvery is the sweep frequency. Defaults to 1 hour per AC#1.
	TickEvery time.Duration
	// BatchSize bounds the per-tick batch so a backlog cannot starve
	// graceful shutdown. Defaults to 200.
	BatchSize int
}

// Worker is the dunning tick. Run from a goroutine until ctx is cancelled.
type Worker struct {
	candidates CandidatesLister
	saver      Saver
	courtesy   billingdunning.CourtesyOverride
	policy     PolicyResolver
	clock      func() time.Time
	logger     *slog.Logger
	metrics    *Metrics
	actorID    uuid.UUID
	tickEvery  time.Duration
	batchSize  int
	tracer     trace.Tracer
}

// New constructs a Worker from cfg. Required: Candidates, Saver,
// Courtesy, ActorID. Returns a descriptive error for missing pieces so
// a misconfigured wireup fails fast at boot.
func New(cfg Config) (*Worker, error) {
	if cfg.Candidates == nil {
		return nil, errors.New("dunning: CandidatesLister is required")
	}
	if cfg.Saver == nil {
		return nil, errors.New("dunning: Saver is required")
	}
	if cfg.Courtesy == nil {
		return nil, errors.New("dunning: CourtesyOverride is required (use NoCourtesyOverride for none)")
	}
	if cfg.ActorID == uuid.Nil {
		return nil, errors.New("dunning: ActorID is required (non-zero UUID)")
	}
	if cfg.Policy == nil {
		cfg.Policy = DefaultPolicyResolver
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.TickEvery <= 0 {
		cfg.TickEvery = time.Hour
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 200
	}
	return &Worker{
		candidates: cfg.Candidates,
		saver:      cfg.Saver,
		courtesy:   cfg.Courtesy,
		policy:     cfg.Policy,
		clock:      cfg.Clock,
		logger:     cfg.Logger,
		metrics:    cfg.Metrics,
		actorID:    cfg.ActorID,
		tickEvery:  cfg.TickEvery,
		batchSize:  cfg.BatchSize,
		tracer:     otel.Tracer("github.com/pericles-luz/crm/internal/worker/dunning"),
	}, nil
}

// Run blocks until ctx is cancelled, ticking once per TickEvery. The
// first tick fires immediately so tests do not need to wait for the
// timer. Tick errors are non-fatal — the next tick retries.
func (w *Worker) Run(ctx context.Context) error {
	t := time.NewTicker(w.tickEvery)
	defer t.Stop()
	_ = w.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			_ = w.Tick(ctx)
		}
	}
}

// Tick performs one sweep. Exported so tests, integration harnesses, and
// CLI tools can drive the worker deterministically. The method records
// dunning_tick_latency_seconds on the metrics surface for every call,
// including error paths.
func (w *Worker) Tick(ctx context.Context) error {
	started := w.clock()
	defer func() {
		w.metrics.observeTick(w.clock().Sub(started).Seconds())
	}()

	ctx, span := w.tracer.Start(ctx, "dunning.worker.tick",
		trace.WithSpanKind(trace.SpanKindInternal),
	)
	defer span.End()

	now := started
	candidates, err := w.candidates.ListCandidates(ctx, now, w.batchSize)
	if err != nil {
		span.RecordError(err)
		w.logger.Error("dunning.worker: list candidates failed",
			slog.String("err", err.Error()),
		)
		return fmt.Errorf("list candidates: %w", err)
	}
	span.SetAttributes(attribute.Int("dunning.worker.candidate_count", len(candidates)))

	counts := map[billingdunning.State]int{}
	for _, c := range candidates {
		w.evaluate(ctx, c, now)
		// Re-read final state post-mutation for the gauge.
		counts[c.Row.State()]++
	}
	w.publishStateGauge(counts)
	return nil
}

// OnPaymentConfirmed is the immediate downgrade path used by the C13
// webhook handler. It loads the dunning row for subscriptionID, calls
// MarkPaid, and saves. Idempotent at current.
//
// We accept the post-mark hydrated row from a caller-supplied loader
// rather than re-listing through Candidates: the webhook handler knows
// the exact subscription, so a point read is cheaper than a scan.
//
// Returns dunning.ErrNotFound if the subscription has no dunning row,
// dunning.ErrInvalidTransition if the row is cancelled (the receipt
// itself is handled in the invoice domain), or whatever error the
// loader / saver surfaces.
func (w *Worker) OnPaymentConfirmed(
	ctx context.Context,
	loader func(ctx context.Context, subscriptionID uuid.UUID) (*billingdunning.DunningState, error),
	subscriptionID uuid.UUID,
) error {
	if subscriptionID == uuid.Nil {
		return billingdunning.ErrZeroSubscription
	}
	row, err := loader(ctx, subscriptionID)
	if err != nil {
		return err
	}
	from := row.State()
	now := w.clock()
	if err := row.MarkPaid(now); err != nil {
		return err
	}
	if err := w.saver.Save(ctx, row, w.actorID); err != nil {
		return fmt.Errorf("dunning.worker: save mark-paid: %w", err)
	}
	if from != billingdunning.StateCurrent {
		w.metrics.incTransition(string(from), string(billingdunning.StateCurrent))
		w.logger.Info("dunning.worker: payment confirmed → current",
			slog.String("subscription_id", subscriptionID.String()),
			slog.String("from", string(from)),
		)
	}
	return nil
}

// evaluate processes a single candidate end-to-end:
//
//  1. Resolve the live courtesy override (if any) and apply it when the
//     stored row hasn't caught up. ApplyOverride resets to current.
//  2. With no past-due pending invoice, downgrade to current via
//     MarkPaid (idempotent at current).
//  3. Otherwise, run Escalate with the live override; persist if moved.
func (w *Worker) evaluate(ctx context.Context, c Candidate, now time.Time) {
	logger := w.logger.With(
		slog.String("subscription_id", c.SubscriptionID.String()),
		slog.String("tenant_id", c.TenantID.String()),
	)

	override, ovrErr := w.lookupOverride(ctx, c.TenantID, now)
	if ovrErr != nil {
		logger.Error("dunning.worker: lookup override failed",
			slog.String("err", ovrErr.Error()),
		)
		// Fall through with no override; an override-lookup failure
		// must NOT block escalation (defense in depth: the database is
		// the source of truth and the master can re-grant later).
	}

	// (1) Apply a new override that hasn't been cached on the row yet.
	if override != nil && needsOverrideRefresh(c.Row, override, now) {
		from := c.Row.State()
		if err := c.Row.ApplyOverride(override.Until, override.Reason, now); err != nil {
			logger.Error("dunning.worker: apply override failed",
				slog.String("err", err.Error()),
			)
			return
		}
		if err := w.saver.Save(ctx, c.Row, w.actorID); err != nil {
			logger.Error("dunning.worker: save override failed",
				slog.String("err", err.Error()),
			)
			return
		}
		if from != billingdunning.StateCurrent {
			w.metrics.incTransition(string(from), string(billingdunning.StateCurrent))
		}
		logger.Info("dunning.worker: applied courtesy override",
			slog.Time("until", override.Until),
			slog.String("from", string(from)),
		)
		return
	}

	// (2) No pending past-due invoice → drop to current if not already.
	if c.Pending == nil {
		if c.Row.State() == billingdunning.StateCurrent {
			return
		}
		from := c.Row.State()
		if err := c.Row.MarkPaid(now); err != nil {
			logger.Error("dunning.worker: mark paid failed",
				slog.String("err", err.Error()),
			)
			return
		}
		if err := w.saver.Save(ctx, c.Row, w.actorID); err != nil {
			logger.Error("dunning.worker: save mark-paid failed",
				slog.String("err", err.Error()),
			)
			return
		}
		w.metrics.incTransition(string(from), string(billingdunning.StateCurrent))
		logger.Info("dunning.worker: no pending invoice → current",
			slog.String("from", string(from)),
		)
		return
	}

	// (3) Escalate using live override.
	policy := w.policy(c.PlanID)
	from := c.Row.State()
	moved, err := c.Row.Escalate(now, policy, c.Pending.ID, c.Pending.PeriodStart, override)
	if err != nil {
		logger.Error("dunning.worker: escalate failed",
			slog.String("err", err.Error()),
		)
		return
	}
	if !moved {
		return
	}
	if err := w.saver.Save(ctx, c.Row, w.actorID); err != nil {
		logger.Error("dunning.worker: save escalate failed",
			slog.String("err", err.Error()),
		)
		return
	}
	w.metrics.incTransition(string(from), string(c.Row.State()))
	logger.Info("dunning.worker: escalated",
		slog.String("from", string(from)),
		slog.String("to", string(c.Row.State())),
		slog.Time("invoice_period_start", c.Pending.PeriodStart),
	)
}

// lookupOverride wraps the courtesy port translating ErrNoActiveOverride
// into a nil pointer so the worker logic stays branch-light. Real errors
// surface to the caller and are logged.
func (w *Worker) lookupOverride(ctx context.Context, tenantID uuid.UUID, now time.Time) (*billingdunning.Override, error) {
	got, err := w.courtesy.ActiveFor(ctx, tenantID, now)
	if err != nil {
		if errors.Is(err, billingdunning.ErrNoActiveOverride) {
			return nil, nil
		}
		return nil, err
	}
	return &got, nil
}

// needsOverrideRefresh reports whether the row needs ApplyOverride
// called: a new grant exists OR the stored Until differs from the live
// one. Comparing Until exactly is fine because both come from the same
// adapter — drift never happens within a tick.
func needsOverrideRefresh(row *billingdunning.DunningState, live *billingdunning.Override, now time.Time) bool {
	if !live.Until.After(now) {
		return false
	}
	stored := row.OverrideUntil()
	if stored == nil {
		return true
	}
	return !stored.Equal(live.Until)
}

// publishStateGauge resets the dunning_state_total{state} gauge to the
// counts observed in this tick. We always publish all known state
// labels (including zeroes) so an empty bucket clears the previous
// reading; without that, a state that just emptied would still show its
// last non-zero value until the next non-empty tick.
func (w *Worker) publishStateGauge(counts map[billingdunning.State]int) {
	for _, s := range []billingdunning.State{
		billingdunning.StateCurrent,
		billingdunning.StateWarn,
		billingdunning.StateSuspendedOutbound,
		billingdunning.StateSuspendedFull,
		billingdunning.StateCancelled,
	} {
		w.metrics.setStateCount(string(s), counts[s])
	}
}

// NoCourtesyOverride is the default-off implementation. It always
// returns ErrNoActiveOverride so the worker treats every tenant as
// "no override". Wire this in deployments where masters cannot grant
// free_subscription_period.
type NoCourtesyOverride struct{}

// ActiveFor implements billingdunning.CourtesyOverride.
func (NoCourtesyOverride) ActiveFor(context.Context, uuid.UUID, time.Time) (billingdunning.Override, error) {
	return billingdunning.Override{}, billingdunning.ErrNoActiveOverride
}

var _ billingdunning.CourtesyOverride = NoCourtesyOverride{}
