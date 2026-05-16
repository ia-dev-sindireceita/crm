package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/pericles-luz/crm/internal/billing"
)

// Config bundles Allocator dependencies. The zero value is invalid;
// New returns an error if a required field is missing and fills
// defaults for the rest.
type Config struct {
	// Subscriber is the JetStream port the Allocator pulls deliveries
	// from. Required.
	Subscriber EventSubscriber
	// Plans is the read-side port for plan.monthly_token_quota
	// lookups. Required.
	Plans PlanReader
	// Allocator is the wallet-write port. Required.
	Allocator MonthlyAllocator

	// Clock returns "now". Defaults to time.Now (UTC). Tests inject a
	// fixed clock to assert lag observations.
	Clock func() time.Time
	// Logger is the structured logger. Defaults to slog.Default().
	Logger *slog.Logger
	// Metrics is the Prometheus surface. nil disables metrics (safe).
	Metrics *Metrics

	// NakDelay is the JetStream redelivery delay the Allocator
	// requests on retryable errors (plan lookup, allocate). The broker
	// still respects BackOff and MaxDeliver; this is best-effort.
	// Defaults to 5s.
	NakDelay time.Duration
}

// Allocator is the worker. Run from a goroutine until ctx is cancelled.
type Allocator struct {
	subscriber EventSubscriber
	plans      PlanReader
	allocator  MonthlyAllocator
	clock      func() time.Time
	logger     *slog.Logger
	metrics    *Metrics
	nakDelay   time.Duration
	tracer     trace.Tracer
}

// New constructs an Allocator from cfg. Required: Subscriber, Plans,
// Allocator. Returns a descriptive error for missing pieces so a
// misconfigured wireup fails fast at boot.
func New(cfg Config) (*Allocator, error) {
	if cfg.Subscriber == nil {
		return nil, errors.New("wallet: EventSubscriber is required")
	}
	if cfg.Plans == nil {
		return nil, errors.New("wallet: PlanReader is required")
	}
	if cfg.Allocator == nil {
		return nil, errors.New("wallet: MonthlyAllocator is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.NakDelay <= 0 {
		cfg.NakDelay = 5 * time.Second
	}
	return &Allocator{
		subscriber: cfg.Subscriber,
		plans:      cfg.Plans,
		allocator:  cfg.Allocator,
		clock:      cfg.Clock,
		logger:     cfg.Logger,
		metrics:    cfg.Metrics,
		nakDelay:   cfg.NakDelay,
		tracer:     otel.Tracer("github.com/pericles-luz/crm/internal/worker/wallet"),
	}, nil
}

// Run blocks until ctx is cancelled or the subscriber reports a
// terminal error. Each delivery is dispatched to Handle synchronously
// — the durable JetStream consumer fans out at the broker level by
// running multiple Allocator processes (one per pod), not by spawning
// goroutines per message.
func (a *Allocator) Run(ctx context.Context) error {
	deliveries, errs, err := a.subscriber.Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("wallet: subscribe: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-deliveries:
			if !ok {
				// Subscriber drained. Drain the error channel best-effort
				// so a terminal cause is surfaced.
				select {
				case e := <-errs:
					if e != nil {
						return fmt.Errorf("wallet: subscriber closed: %w", e)
					}
				default:
				}
				return nil
			}
			a.Handle(ctx, d)
		case e := <-errs:
			if e != nil {
				return fmt.Errorf("wallet: subscriber error: %w", e)
			}
		}
	}
}

// Handle processes one delivery end-to-end: decode → plan lookup →
// allocate → ack/nak. Exported so tests can drive the worker without
// running the full subscription loop. Errors are observed via metrics
// + logs; Handle never returns an error so the caller (Run) does not
// have to special-case retryable vs fatal.
func (a *Allocator) Handle(ctx context.Context, d Delivery) {
	start := a.clock()
	ctx, span := a.tracer.Start(ctx, "wallet.allocator.handle",
		trace.WithSpanKind(trace.SpanKindConsumer),
	)
	defer span.End()
	defer func() {
		a.metrics.observeDuration(a.clock().Sub(start).Seconds())
	}()

	msgID := d.MsgID()
	if msgID == "" {
		// Missing Nats-Msg-Id is a publisher bug. We MUST not allocate
		// without an idempotency key — a redelivery would double-credit
		// the wallet. Ack to drop the message; an operator catches the
		// regression via wallet_allocation_total{outcome="missing_msg_id"}.
		a.metrics.incAllocation(OutcomeMissingMsgID)
		a.logger.Error("wallet.allocator: dropping delivery without Nats-Msg-Id (publisher bug)")
		span.SetAttributes(attribute.String("wallet.outcome", OutcomeMissingMsgID))
		_ = d.Ack(ctx)
		return
	}
	logger := a.logger.With(slog.String("nats_msg_id", msgID))
	span.SetAttributes(attribute.String("nats.msg_id", msgID))

	var ev Event
	if err := json.Unmarshal(d.Data(), &ev); err != nil {
		// Malformed payload: cannot retry, cannot dedup. Ack so a
		// poison pill does not loop and let the operator alert on
		// failed_decode.
		a.metrics.incAllocation(OutcomeFailedDecode)
		logger.Error("wallet.allocator: decode failed",
			slog.String("err", err.Error()),
		)
		span.SetAttributes(attribute.String("wallet.outcome", OutcomeFailedDecode))
		span.RecordError(err)
		_ = d.Ack(ctx)
		return
	}
	logger = logger.With(
		slog.String("subscription_id", ev.SubscriptionID.String()),
		slog.String("tenant_id", ev.TenantID.String()),
		slog.String("plan_id", ev.PlanID.String()),
		slog.Time("new_period_start", ev.NewPeriodStart),
	)
	span.SetAttributes(
		attribute.String("subscription.id", ev.SubscriptionID.String()),
		attribute.String("tenant.id", ev.TenantID.String()),
		attribute.String("plan.id", ev.PlanID.String()),
	)

	// Observe lag against the snapshot clock — for tests the clock is
	// fixed and the lag is deterministic; in prod the histogram tracks
	// CA #3 (< 30s p99).
	a.metrics.observeLag(start.Sub(ev.NewPeriodStart).Seconds())

	plan, err := a.plans.GetPlanByID(ctx, ev.PlanID)
	if err != nil {
		// Plan lookup failure is retryable (transient DB) unless the
		// plan does not exist — that is fatal-for-this-message. Both
		// branches Nak so JetStream redelivers; the broker drops the
		// message into DLQ once MaxDeliver is reached and the operator
		// alerts on failed_plan.
		a.metrics.incAllocation(OutcomeFailedPlan)
		logger.Error("wallet.allocator: plan lookup failed",
			slog.String("err", err.Error()),
		)
		span.SetAttributes(attribute.String("wallet.outcome", OutcomeFailedPlan))
		span.RecordError(err)
		_ = d.Nak(ctx, a.nakDelay)
		return
	}
	span.SetAttributes(attribute.Int64("plan.monthly_token_quota", plan.MonthlyTokenQuota))

	allocated, err := a.allocator.AllocateMonthlyQuota(ctx, ev.TenantID, ev.NewPeriodStart, plan.MonthlyTokenQuota, msgID)
	if err != nil {
		a.metrics.incAllocation(OutcomeFailedAllocate)
		logger.Error("wallet.allocator: allocate failed",
			slog.String("err", err.Error()),
		)
		span.SetAttributes(attribute.String("wallet.outcome", OutcomeFailedAllocate))
		span.RecordError(err)
		_ = d.Nak(ctx, a.nakDelay)
		return
	}

	outcome := OutcomeAllocated
	if !allocated {
		outcome = OutcomeSkippedDuplicate
	}
	a.metrics.incAllocation(outcome)
	span.SetAttributes(attribute.String("wallet.outcome", outcome))
	if allocated {
		logger.Info("wallet.allocator: monthly quota allocated",
			slog.Int64("amount", plan.MonthlyTokenQuota),
		)
	} else {
		logger.Info("wallet.allocator: duplicate delivery; allocation already credited",
			slog.Int64("amount", plan.MonthlyTokenQuota),
		)
	}
	_ = d.Ack(ctx)
}

// guard so a compile-time mismatch between billing.PlanCatalog and the
// worker's PlanReader is caught by the test build.
var _ PlanReader = (billing.PlanCatalog)(nil)
