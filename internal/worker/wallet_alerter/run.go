package wallet_alerter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// DefaultAckWait is the JetStream redelivery timeout. The worker's
// happy path is "decode → dedup check → HTTP POST → ack", which Slack
// serves in well under a second; 15s gives generous headroom for a
// slow webhook and matches the spirit of the mediascan-worker default
// (30s for multi-MB ClamAV scans).
const DefaultAckWait = 15 * time.Second

// Subscriber is the narrow slice of *natsadapter.SDKAdapter the worker
// consumes. Defined here (not in the adapter package) so unit tests can
// inject in-memory fakes without touching the SDK adapter's public API.
//
// EnsureStream is on the same surface because the worker owns the
// stream definition (Subject, retention defaults). cmd entrypoints
// call it via the same shim that mediascan-worker uses.
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
// argument is Delivery (the narrow port, not the concrete adapter
// type) so tests can drive the wiring path with in-memory deliveries.
type HandlerFunc func(ctx context.Context, d Delivery) error

// RunConfig bundles the env-driven knobs Run consumes. Only Notifier
// and Subscriber are mandatory; everything else has a sensible default.
type RunConfig struct {
	// Notifier is the outbound Slack adapter. When the operator has not
	// configured SLACK_ALERTS_WEBHOOK_URL the upstream factory passes a
	// no-op Notifier (the Slack webhook adapter with an empty URL does
	// this naturally). Run logs a one-shot warning at boot in that case
	// so the operator sees the degraded posture.
	Notifier Notifier

	// NotifyDegraded is true when the caller wired Notifier as a no-op
	// because SLACK_ALERTS_WEBHOOK_URL was empty. Used only to emit the
	// boot-time warning; it does NOT change Notifier dispatch.
	NotifyDegraded bool

	// Logger is the structured logger.
	Logger *slog.Logger

	// Clock is optional; defaults to SystemClock.
	Clock Clock

	// DedupTTL is optional; defaults to DefaultDedupTTL.
	DedupTTL time.Duration

	// AckWait is optional; defaults to DefaultAckWait.
	AckWait time.Duration
}

// Run wires the alerter onto subscriber and blocks until ctx is done.
// Returns nil on a clean shutdown; any wiring error is wrapped with a
// stage label so an operator can triage to a specific step.
func Run(ctx context.Context, sub Subscriber, cfg RunConfig) error {
	if sub == nil {
		return errors.New("wallet_alerter: Subscriber is required")
	}
	if cfg.Notifier == nil {
		return errors.New("wallet_alerter: Notifier is required")
	}
	if cfg.Logger == nil {
		return errors.New("wallet_alerter: Logger is required")
	}
	if cfg.AckWait <= 0 {
		cfg.AckWait = DefaultAckWait
	}

	if cfg.NotifyDegraded {
		// AC #3 of SIN-62905: SLACK_ALERTS_WEBHOOK_URL ausente → worker
		// loga warning e segue (não crash; alerta apenas degrada).
		cfg.Logger.Warn("wallet_alerter: SLACK_ALERTS_WEBHOOK_URL not configured; alerts will be silently dropped")
	}

	if err := sub.EnsureStream(StreamName, []string{Subject}); err != nil {
		return fmt.Errorf("wallet_alerter: ensure stream: %w", err)
	}

	a, err := New(cfg.Notifier, cfg.Logger, cfg.Clock, cfg.DedupTTL)
	if err != nil {
		return fmt.Errorf("wallet_alerter: build alerter: %w", err)
	}

	subscription, err := sub.Subscribe(ctx, Subject, QueueName, DurableName, cfg.AckWait,
		func(c context.Context, d Delivery) error {
			return a.Handle(c, d)
		},
	)
	if err != nil {
		return fmt.Errorf("wallet_alerter: subscribe: %w", err)
	}

	cfg.Logger.Info("wallet_alerter: ready",
		"subject", Subject,
		"queue", QueueName,
		"durable", DurableName,
		"stream", StreamName,
		"ack_wait", cfg.AckWait.String(),
		"dedup_ttl", durationOrDefault(cfg.DedupTTL, DefaultDedupTTL).String(),
		"notify_degraded", cfg.NotifyDegraded,
	)

	<-ctx.Done()

	cfg.Logger.Info("wallet_alerter: shutting down")
	_ = subscription.Drain()
	if err := sub.Drain(); err != nil {
		cfg.Logger.Warn("wallet_alerter: subscriber drain failed", "err", err.Error())
	}
	return nil
}

func durationOrDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}
