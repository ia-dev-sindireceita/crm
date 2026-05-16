package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/pericles-luz/crm/internal/billing"
)

// SubjectSubscriptionRenewed is the JetStream subject the renewer
// publishes to. Consumers (C6 wallet allocator, C8 audit log) subscribe
// to the same string.
const SubjectSubscriptionRenewed = "subscription.renewed"

// Event is the JSON payload emitted on subscription.renewed. The shape
// is the contract C6/C8 consume; once a consumer ships, additive fields
// only. Times are RFC3339 UTC; ints are cents BRL.
type Event struct {
	SubscriptionID    uuid.UUID `json:"subscription_id"`
	TenantID          uuid.UUID `json:"tenant_id"`
	PlanID            uuid.UUID `json:"plan_id"`
	InvoiceID         uuid.UUID `json:"invoice_id"`
	PreviousPeriodEnd time.Time `json:"previous_period_end"`
	NewPeriodStart    time.Time `json:"new_period_start"`
	NewPeriodEnd      time.Time `json:"new_period_end"`
	AmountCentsBRL    int       `json:"amount_cents_brl"`
	RenewedAt         time.Time `json:"renewed_at"`
}

// Config bundles Renewer dependencies. The zero value is invalid; New()
// returns an error if a required field is missing and fills defaults
// for the rest.
type Config struct {
	// Due lists active subscriptions whose period has elapsed.
	Due DueSubscriptionsLister
	// Renewer atomically advances one subscription and creates its
	// pending invoice.
	Renewer SubscriptionRenewer
	// Publisher publishes subscription.renewed events on JetStream.
	Publisher EventPublisher
	// Clock returns "now". Defaults to time.Now (UTC).
	Clock func() time.Time
	// Logger is the structured logger. Defaults to slog.Default().
	Logger *slog.Logger
	// Metrics is the Prometheus surface. nil disables metrics (safe).
	Metrics *Metrics
	// ActorID is the audit actor recorded by WithMasterOps. The system
	// uses a fixed UUID for the renewer worker so audit consumers can
	// distinguish system writes from operator writes.
	ActorID uuid.UUID
	// TickEvery is the sweep frequency. Defaults to 1 hour — every
	// subscription falls due at most an hour late, which is the same
	// budget the wallet allocator (C6) tolerates.
	TickEvery time.Duration
	// BatchSize bounds the per-tick batch so a backlog cannot starve
	// graceful shutdown. Defaults to 100.
	BatchSize int
	// PublishMaxRetries is the upper bound on per-event publish retries
	// before the renewer gives up on this subscription for this tick
	// (the row stays unrenewed and is retried on the next tick).
	// Defaults to 5.
	PublishMaxRetries int
	// PublishBaseDelay is the exponential-backoff base. Defaults to
	// 100ms. Cap is PublishMaxDelay.
	PublishBaseDelay time.Duration
	// PublishMaxDelay caps the per-retry delay. Defaults to 5s.
	PublishMaxDelay time.Duration
}

// Renewer is the worker. Run from a goroutine until ctx is cancelled.
type Renewer struct {
	due               DueSubscriptionsLister
	renewer           SubscriptionRenewer
	publisher         EventPublisher
	clock             func() time.Time
	logger            *slog.Logger
	metrics           *Metrics
	actorID           uuid.UUID
	tickEvery         time.Duration
	batchSize         int
	publishMaxRetries int
	publishBaseDelay  time.Duration
	publishMaxDelay   time.Duration
	tracer            trace.Tracer
}

// New constructs a Renewer from cfg. Required: Due, Renewer, Publisher,
// ActorID. Returns a descriptive error for missing pieces so a
// misconfigured wireup fails fast at boot.
func New(cfg Config) (*Renewer, error) {
	if cfg.Due == nil {
		return nil, errors.New("billing: DueSubscriptionsLister is required")
	}
	if cfg.Renewer == nil {
		return nil, errors.New("billing: SubscriptionRenewer is required")
	}
	if cfg.Publisher == nil {
		return nil, errors.New("billing: EventPublisher is required")
	}
	if cfg.ActorID == uuid.Nil {
		return nil, errors.New("billing: ActorID is required (non-zero UUID)")
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
		cfg.BatchSize = 100
	}
	if cfg.PublishMaxRetries <= 0 {
		cfg.PublishMaxRetries = 5
	}
	if cfg.PublishBaseDelay <= 0 {
		cfg.PublishBaseDelay = 100 * time.Millisecond
	}
	if cfg.PublishMaxDelay <= 0 {
		cfg.PublishMaxDelay = 5 * time.Second
	}
	return &Renewer{
		due:               cfg.Due,
		renewer:           cfg.Renewer,
		publisher:         cfg.Publisher,
		clock:             cfg.Clock,
		logger:            cfg.Logger,
		metrics:           cfg.Metrics,
		actorID:           cfg.ActorID,
		tickEvery:         cfg.TickEvery,
		batchSize:         cfg.BatchSize,
		publishMaxRetries: cfg.PublishMaxRetries,
		publishBaseDelay:  cfg.PublishBaseDelay,
		publishMaxDelay:   cfg.PublishMaxDelay,
		tracer:            otel.Tracer("github.com/pericles-luz/crm/internal/worker/billing"),
	}, nil
}

// Run blocks until ctx is cancelled, ticking once per TickEvery. The
// first tick fires immediately so tests do not need to wait for the
// timer. Tick errors are non-fatal — the next tick retries.
func (r *Renewer) Run(ctx context.Context) error {
	t := time.NewTicker(r.tickEvery)
	defer t.Stop()
	_ = r.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			_ = r.Tick(ctx)
		}
	}
}

// Tick performs one sweep. Exported so tests and CLI tools can drive
// the renewer deterministically.
func (r *Renewer) Tick(ctx context.Context) error {
	ctx, span := r.tracer.Start(ctx, "billing.renewer.tick",
		trace.WithSpanKind(trace.SpanKindInternal),
	)
	defer span.End()

	now := r.clock()
	due, err := r.due.ListDueSubscriptions(ctx, now, r.batchSize)
	if err != nil {
		span.RecordError(err)
		r.logger.Error("billing.renewer: list due subscriptions failed",
			slog.String("err", err.Error()),
		)
		return fmt.Errorf("list due subscriptions: %w", err)
	}
	span.SetAttributes(attribute.Int("billing.renewer.due_count", len(due)))

	for _, d := range due {
		r.renewOne(ctx, d, now)
	}
	return nil
}

// renewOne processes a single subscription end-to-end: atomic DB write,
// then publish (with retry/backoff). On publish-exhaustion we leave the
// subscription advanced in the DB and let the next sweep see no due
// rows; the message is then permanently undelivered for this period —
// the operator alerts on billing_renewer_run_total{outcome="error"}.
// Tests cover both branches.
func (r *Renewer) renewOne(ctx context.Context, d DueSubscription, now time.Time) {
	logger := r.logger.With(
		slog.String("subscription_id", d.ID.String()),
		slog.String("tenant_id", d.TenantID.String()),
	)
	res, err := r.renewer.RenewSubscription(ctx, d.ID, d.CurrentPeriodEnd, d.PlanPriceCents, r.actorID, now)
	if err != nil {
		if errors.Is(err, billing.ErrInvoiceAlreadyExists) {
			r.metrics.incRun(OutcomeSkippedAlreadyDone)
			logger.Info("billing.renewer: invoice already exists for period; skipping")
			return
		}
		r.metrics.incRun(OutcomeError)
		logger.Error("billing.renewer: renew failed",
			slog.String("err", err.Error()),
		)
		return
	}

	msgID := buildMsgID(d.ID, res.NewPeriodStart)
	body, err := json.Marshal(Event{
		SubscriptionID:    res.Subscription.ID(),
		TenantID:          res.Subscription.TenantID(),
		PlanID:            res.Subscription.PlanID(),
		InvoiceID:         res.Invoice.ID(),
		PreviousPeriodEnd: d.CurrentPeriodEnd,
		NewPeriodStart:    res.NewPeriodStart,
		NewPeriodEnd:      res.NewPeriodEnd,
		AmountCentsBRL:    res.Invoice.AmountCentsBRL(),
		RenewedAt:         now,
	})
	if err != nil {
		// json.Marshal on a pure-struct payload can only fail on bug
		// (uuid.UUID, time.Time, int all marshal). Count and log so an
		// unexpected regression is visible.
		r.metrics.incRun(OutcomeError)
		logger.Error("billing.renewer: marshal event failed",
			slog.String("err", err.Error()),
		)
		return
	}

	if err := r.publishWithBackoff(ctx, msgID, body); err != nil {
		r.metrics.incRun(OutcomeError)
		logger.Error("billing.renewer: publish exhausted retries",
			slog.String("err", err.Error()),
			slog.String("nats_msg_id", msgID),
		)
		return
	}

	r.metrics.incRun(OutcomeSuccess)
	r.metrics.incInvoice(string(res.Invoice.State()))
	logger.Info("billing.renewer: subscription renewed",
		slog.String("invoice_id", res.Invoice.ID().String()),
		slog.String("nats_msg_id", msgID),
		slog.Time("new_period_start", res.NewPeriodStart),
		slog.Time("new_period_end", res.NewPeriodEnd),
	)
}

// publishWithBackoff implements per-event exponential backoff with a
// cap, so a transient JetStream blip does not poison the worker. Returns
// the last publish error when the retries are exhausted.
func (r *Renewer) publishWithBackoff(ctx context.Context, msgID string, body []byte) error {
	delay := r.publishBaseDelay
	var lastErr error
	for i := 0; i < r.publishMaxRetries; i++ {
		if err := r.publisher.Publish(ctx, SubjectSubscriptionRenewed, msgID, body); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if i == r.publishMaxRetries-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
		if delay > r.publishMaxDelay {
			delay = r.publishMaxDelay
		}
	}
	return lastErr
}

// buildMsgID builds the JetStream Nats-Msg-Id for the renewal event.
// Format: "{subscription_id}:{new_period_start_iso}" where the period
// start is rendered as RFC3339 UTC. The format is deterministic so a
// second renewer producing the same advancement dedups server-side.
func buildMsgID(subID uuid.UUID, newPeriodStart time.Time) string {
	return subID.String() + ":" + newPeriodStart.UTC().Format(time.RFC3339Nano)
}
