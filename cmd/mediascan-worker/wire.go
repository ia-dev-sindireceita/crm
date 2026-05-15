// Wire is the testable core of run() ([SIN-62826]). It builds the
// worker.Handler from injected ports, ensures the JetStream stream
// exists, binds the queue subscription, logs the security posture
// once at boot, and blocks on ctx until SIGTERM. Every external
// collaborator is a port-shaped dep — the env-to-port conversion
// stays in run() so Wire's tests do not need a Postgres/NATS/MinIO
// rig.
//
// The NATSAdapter / Subscription interfaces below are intentionally
// the narrowest slice of *natsadapter.SDKAdapter the wiring path
// touches: EnsureStream, Subscribe, Drain (on both the adapter and
// the returned subscription). Production wraps the concrete adapter
// via natsAdapterShim so cmd/mediascan-worker stays vendor-SDK-free
// (no `github.com/nats-io/nats.go` import here).

package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	"github.com/pericles-luz/crm/internal/media/alert"
	"github.com/pericles-luz/crm/internal/media/quarantine"
	"github.com/pericles-luz/crm/internal/media/scanner"
	"github.com/pericles-luz/crm/internal/media/worker"
)

// SubscribeAckWait is the JetStream redelivery timeout for the worker
// subscription. Matches the previous inline literal in run() ([SIN-62804]):
// > the slowest scan we expect to tolerate (default 30s for ClamAV
// > INSTREAM on multi-MB blobs).
const SubscribeAckWait = 30 * time.Second

// HandleFunc is the per-delivery callback Wire installs through
// NATSAdapter.Subscribe. The argument is worker.Delivery (the narrow
// port, not the concrete *natsadapter.Delivery) so tests can drive the
// wiring path with in-memory deliveries that do not depend on the
// NATS SDK.
type HandleFunc func(ctx context.Context, d worker.Delivery) error

// Subscription is the slice of the returned JetStream subscription
// Wire calls at shutdown. *natsgo.Subscription satisfies it via the
// natsAdapterShim wrapper; tests provide an in-process fake.
type Subscription interface {
	Drain() error
}

// NATSAdapter is the narrow slice of *natsadapter.SDKAdapter that
// Wire consumes. Defined here (not in the adapter package) so unit
// tests can inject in-memory fakes without touching the SDK adapter's
// public API.
type NATSAdapter interface {
	EnsureStream(name string, subjects []string) error
	Subscribe(
		ctx context.Context,
		subject, queue, durable string,
		ackWait time.Duration,
		handler HandleFunc,
	) (Subscription, error)
	Drain() error
}

// Deps is the typed dependency set Wire consumes.
//
// Each field is a port-shaped collaborator already built by run() from
// the environment (see main.go). Quarantiner and Alerter are
// nil-allowed — the worker tolerates missing defense-in-depth in
// non-production deploys, exactly as it did before this refactor.
type Deps struct {
	Cfg       config
	Logger    *slog.Logger
	Scanner   scanner.MediaScanner
	Store     scanner.MessageMediaStore
	NATS      NATSAdapter
	Publisher worker.Publisher

	// Quarantiner is nil when MINIO_ENDPOINT is empty (the local-fs
	// dev path). Worker keeps logging infected verdicts in that case
	// but does not move the blob.
	Quarantiner quarantine.Quarantiner

	// Alerter is nil when SLACK_WEBHOOK_URL is empty. Worker still
	// logs at ERROR level so a Loki query can detect infected verdicts
	// without Slack.
	Alerter alert.Alerter
}

// Wire ties the deps into a running worker process and blocks on ctx.
// A nil return means ctx was cancelled and shutdown completed cleanly;
// any error is wrapped with a stage label so an operator can triage
// the boot failure to a specific step.
func Wire(ctx context.Context, deps Deps) error {
	if err := deps.NATS.EnsureStream(deps.Cfg.streamName, []string{
		worker.SubjectRequested,
		worker.SubjectCompleted,
	}); err != nil {
		return fmt.Errorf("ensure stream: %w", err)
	}

	handler, err := worker.New(deps.Scanner, deps.Store, deps.Publisher, deps.Logger)
	if err != nil {
		return fmt.Errorf("worker.New: %w", err)
	}
	// Defense-in-depth wiring ([SIN-62805] F2-05d). The worker keeps
	// the Quarantiner / Alerter fields exported so they can be flipped
	// on after New — preserved here to keep the call shape unchanged.
	handler.Quarantiner = deps.Quarantiner
	handler.Alerter = deps.Alerter

	// Concurrency is enforced via a buffered semaphore so the
	// QueueSubscribe callback does not spawn unbounded goroutines on
	// burst. AckWait matches the previous inline 30s literal.
	sem := make(chan struct{}, deps.Cfg.concurrency)

	sub, err := deps.NATS.Subscribe(ctx, worker.SubjectRequested,
		deps.Cfg.queueName, deps.Cfg.durableName, SubscribeAckWait,
		func(c context.Context, d worker.Delivery) error {
			sem <- struct{}{}
			defer func() { <-sem }()
			return handler.Handle(c, d)
		},
	)
	if err != nil {
		return fmt.Errorf("nats subscribe: %w", err)
	}

	// Log the security posture so an operator can audit the deploy
	// without grepping env. Paths only; never the secret material.
	deps.Logger.Info("mediascan-worker ready",
		"nats", deps.Cfg.natsURL,
		"stream", deps.Cfg.streamName,
		"queue", deps.Cfg.queueName,
		"concurrency", deps.Cfg.concurrency,
		"auth", natsAuthMode(deps.Cfg),
		"tls_ca", deps.Cfg.natsTLSCAFile,
		"mtls", deps.Cfg.natsTLSCertFile != "" && deps.Cfg.natsTLSKeyFile != "",
		"insecure", deps.Cfg.natsInsecure,
		"quarantiner", handler.Quarantiner != nil,
		"alerter", handler.Alerter != nil,
		"minio_creds", minioCredsMode(deps.Cfg),
		"minio_creds_refresh", deps.Cfg.minioCredsRefresh.String(),
	)

	<-ctx.Done()

	// Stop accepting new deliveries; in-flight ones get up to AckWait
	// to drain. Drain the NATS conn so the broker sees us leave
	// cleanly.
	deps.Logger.Info("mediascan-worker shutting down")
	_ = sub.Drain()
	if err := deps.NATS.Drain(); err != nil {
		deps.Logger.Warn("nats drain", "err", err.Error())
	}
	return nil
}

// natsAdapterShim wraps *natsadapter.SDKAdapter so it satisfies the
// NATSAdapter interface Wire consumes. The shim is the only place
// that touches the concrete adapter — Wire and its unit tests stay
// decoupled from the NATS SDK type.
type natsAdapterShim struct{ a *natsadapter.SDKAdapter }

// Compile-time fence: *natsAdapterShim must satisfy NATSAdapter.
var _ NATSAdapter = (*natsAdapterShim)(nil)

func (n *natsAdapterShim) EnsureStream(name string, subjects []string) error {
	return n.a.EnsureStream(name, subjects)
}

func (n *natsAdapterShim) Subscribe(
	ctx context.Context,
	subject, queue, durable string,
	ackWait time.Duration,
	handler HandleFunc,
) (Subscription, error) {
	// *natsadapter.Delivery satisfies worker.Delivery, so the typed
	// HandlerFunc the SDK adapter expects is a thin re-wrap of the
	// Wire-shaped HandleFunc.
	return n.a.Subscribe(ctx, subject, queue, durable, ackWait,
		func(c context.Context, d *natsadapter.Delivery) error {
			return handler(c, d)
		},
	)
}

func (n *natsAdapterShim) Drain() error {
	return n.a.Drain()
}
