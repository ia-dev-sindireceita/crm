package aiassistinvalidator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Config bundles Worker dependencies. The zero value is invalid; New
// returns an error if a required field is missing and fills defaults
// for the rest. The shapes match wallet's worker (SIN-62881) so the
// cmd/server wireup pattern is identical.
type Config struct {
	// Subscriber is the JetStream port the Worker pulls deliveries from.
	// Required.
	Subscriber EventSubscriber
	// Invalidator is the aiassist use case the Worker calls on each
	// delivery. Required.
	Invalidator Invalidator
	// Clock returns "now". Defaults to time.Now (UTC). Tests inject a
	// fixed clock to assert lag observations.
	Clock func() time.Time
	// Logger is the structured logger. Defaults to slog.Default().
	Logger *slog.Logger
	// Metrics is the optional Prometheus surface. nil disables.
	Metrics *Metrics
	// NakDelay is the JetStream redelivery delay the Worker requests on
	// retryable errors (Invalidate failure). The broker respects its
	// own BackOff curve first; this is best-effort. Defaults to 5s.
	NakDelay time.Duration
}

// Worker is the cache-invalidation consumer. Run from a goroutine
// until ctx is cancelled.
type Worker struct {
	subscriber  EventSubscriber
	invalidator Invalidator
	clock       func() time.Time
	logger      *slog.Logger
	metrics     *Metrics
	nakDelay    time.Duration
}

// New constructs a Worker from cfg.
func New(cfg Config) (*Worker, error) {
	if cfg.Subscriber == nil {
		return nil, errors.New("aiassistinvalidator: EventSubscriber is required")
	}
	if cfg.Invalidator == nil {
		return nil, errors.New("aiassistinvalidator: Invalidator is required")
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
	return &Worker{
		subscriber:  cfg.Subscriber,
		invalidator: cfg.Invalidator,
		clock:       cfg.Clock,
		logger:      cfg.Logger,
		metrics:     cfg.Metrics,
		nakDelay:    cfg.NakDelay,
	}, nil
}

// Run blocks until ctx is cancelled or the subscriber reports a
// terminal error. Each delivery is processed synchronously — the
// durable JetStream consumer fans out at the broker level.
func (w *Worker) Run(ctx context.Context) error {
	deliveries, errs, err := w.subscriber.Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("aiassistinvalidator: subscribe: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-deliveries:
			if !ok {
				select {
				case e := <-errs:
					if e != nil {
						return fmt.Errorf("aiassistinvalidator: subscriber closed: %w", e)
					}
				default:
				}
				return nil
			}
			w.Handle(ctx, d)
		case e := <-errs:
			if e != nil {
				return fmt.Errorf("aiassistinvalidator: subscriber error: %w", e)
			}
		}
	}
}

// Handle processes one delivery end-to-end. Exported so tests can drive
// the worker without running the subscription loop. Handle never
// returns an error so the caller (Run) does not have to special-case
// retryable vs fatal — outcomes are observed via metrics + logs and
// the per-delivery Ack/Nak decision encodes the disposition.
func (w *Worker) Handle(ctx context.Context, d Delivery) {
	start := w.clock()
	defer func() {
		w.metrics.observeDuration(w.clock().Sub(start).Seconds())
	}()

	msgID := d.MsgID()
	logger := w.logger.With(slog.String("nats_msg_id", msgID))

	var ev Event
	if err := json.Unmarshal(d.Data(), &ev); err != nil {
		// Malformed payload: cannot retry usefully. Ack so the poison
		// pill does not loop; the operator alerts on failed_decode.
		w.metrics.incOutcome(OutcomeFailedDecode)
		logger.Error("aiassistinvalidator: decode failed", slog.String("err", err.Error()))
		_ = d.Ack(ctx)
		return
	}
	if ev.TenantID == uuid.Nil || ev.ConversationID == uuid.Nil {
		// Publisher bug: cannot resolve the (tenant, conversation)
		// pair. Ack so the message does not loop; an operator catches
		// the regression via the missing_id outcome.
		w.metrics.incOutcome(OutcomeMissingIDs)
		logger.Error("aiassistinvalidator: missing tenant/conversation id",
			slog.String("tenant_id", ev.TenantID.String()),
			slog.String("conversation_id", ev.ConversationID.String()),
		)
		_ = d.Ack(ctx)
		return
	}
	logger = logger.With(
		slog.String("tenant_id", ev.TenantID.String()),
		slog.String("conversation_id", ev.ConversationID.String()),
	)

	if err := w.invalidator.Invalidate(ctx, ev.TenantID, ev.ConversationID); err != nil {
		w.metrics.incOutcome(OutcomeFailedInvalidate)
		logger.Error("aiassistinvalidator: invalidate failed", slog.String("err", err.Error()))
		_ = d.Nak(ctx, w.nakDelay)
		return
	}
	w.metrics.incOutcome(OutcomeInvalidated)
	logger.Info("aiassistinvalidator: AISummary invalidated")
	_ = d.Ack(ctx)
}
