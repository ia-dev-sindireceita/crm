package funnel_engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/pericles-luz/crm/internal/funnel/engine"
)

// DefaultAckWait is the JetStream redelivery timeout. The worker's
// happy path is "decode → resolve → maybe-move → record", all bounded
// by Postgres roundtrips; 30s gives generous headroom for the
// resolver query and the funnel transition INSERT under load.
const DefaultAckWait = 30 * time.Second

// Subscriber is the narrow slice of *natsadapter.SDKAdapter the worker
// consumes. Defined here (not in the adapter package) so unit tests
// can inject in-memory fakes without touching the SDK adapter's
// public API.
//
// EnsureStream is on the same surface because the worker owns the
// stream definition (Subject, retention defaults).
type Subscriber interface {
	EnsureStream(name string, subjects []string) error
	Subscribe(
		ctx context.Context,
		subject, queue, durable string,
		ackWait time.Duration,
		handler HandlerFunc,
	) (Subscription, error)
	Drain() error
}

// Subscription is the slice of the returned JetStream subscription
// Run calls at shutdown.
type Subscription interface {
	Drain() error
}

// HandlerFunc is the per-delivery callback Subscribe installs. The
// argument is [Delivery] (the narrow port, not the concrete adapter
// type) so tests can drive the wiring path with in-memory deliveries.
type HandlerFunc func(ctx context.Context, d Delivery) error

// RunConfig bundles the env-driven knobs Run consumes. Engine and
// Logger are mandatory; AckWait defaults to [DefaultAckWait].
type RunConfig struct {
	// Engine is the rule-application core. Required.
	Engine *engine.Engine

	// Logger is the structured logger. Required.
	Logger *slog.Logger

	// AckWait is optional; defaults to DefaultAckWait.
	AckWait time.Duration
}

// Run wires the consumer onto subscriber and blocks until ctx is done.
// Returns nil on a clean shutdown; any wiring error is wrapped with a
// stage label so an operator can triage to a specific step.
func Run(ctx context.Context, sub Subscriber, cfg RunConfig) error {
	if sub == nil {
		return errors.New("funnel_engine: Subscriber is required")
	}
	if cfg.Engine == nil {
		return errors.New("funnel_engine: Engine is required")
	}
	if cfg.Logger == nil {
		return errors.New("funnel_engine: Logger is required")
	}
	if cfg.AckWait <= 0 {
		cfg.AckWait = DefaultAckWait
	}

	if err := sub.EnsureStream(engine.StreamName, []string{engine.Subject}); err != nil {
		return fmt.Errorf("funnel_engine: ensure stream: %w", err)
	}

	consumer, err := NewConsumer(cfg.Engine, cfg.Logger)
	if err != nil {
		return fmt.Errorf("funnel_engine: build consumer: %w", err)
	}

	subscription, err := sub.Subscribe(ctx, engine.Subject, engine.QueueName, engine.DurableName, cfg.AckWait,
		func(c context.Context, d Delivery) error {
			return consumer.Handle(c, d)
		},
	)
	if err != nil {
		return fmt.Errorf("funnel_engine: subscribe: %w", err)
	}

	cfg.Logger.Info("funnel_engine: ready",
		"subject", engine.Subject,
		"queue", engine.QueueName,
		"durable", engine.DurableName,
		"stream", engine.StreamName,
		"ack_wait", cfg.AckWait.String(),
	)

	<-ctx.Done()

	cfg.Logger.Info("funnel_engine: shutting down")
	_ = subscription.Drain()
	if err := sub.Drain(); err != nil {
		cfg.Logger.Warn("funnel_engine: subscriber drain failed", "err", err.Error())
	}
	return nil
}
