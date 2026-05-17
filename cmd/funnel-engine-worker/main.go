// funnel-engine-worker is the standalone process described in
// SIN-62960 (Fase 4 funnel rule engine — NATS consumer, child of
// SIN-62197): it subscribes to the `inbox.messages.received` JetStream
// subject, resolves the per-tenant rules effective for the inbound
// message, evaluates triggers, and dispatches matched actions through
// the funnel service (move_to_stage today).
//
// The worker package (internal/worker/funnel_engine) owns the
// per-delivery dispatch logic; the engine package
// (internal/funnel/engine) owns the resolve + evaluate + apply +
// record pipeline. This entrypoint only:
//
//  1. translates the environment into ports and adapters,
//  2. mounts a Prometheus /metrics listener so operators can scrape
//     funnel_messages_evaluated_total / funnel_rules_matched_total /
//     funnel_actions_applied_total / funnel_evaluation_latency_seconds,
//  3. blocks on SIGINT/SIGTERM until shutdown.
//
// Configuration is read from the environment (12-factor):
//
//	DATABASE_URL                    mandatory — runtime pool DSN.
//	NATS_URL                        mandatory, e.g. tls://nats.example:4222
//	NATS_NAME                       optional, default "crm-funnel-engine-worker".
//	NATS_CONNECT_TIMEOUT            optional Go duration, default 10s.
//	FUNNEL_ENGINE_METRICS_ADDR      optional listen addr, default :9402.
//	FUNNEL_ENGINE_ACK_WAIT          optional Go duration, default 30s.
//
// NATS auth + TLS hardening mirrors cmd/wallet-alerter-worker:
//
//	NATS_CREDS_FILE                 preferred for production.
//	NATS_NKEY_FILE                  alternative.
//	NATS_TOKEN                      legacy / dev only.
//	NATS_TLS_CA                     required when scheme is tls:// or wss://.
//	NATS_TLS_CERT, NATS_TLS_KEY     optional mTLS pair.
//	NATS_INSECURE=1                 opt-out of secure default.
//
// Shutdown contract: SIGINT and SIGTERM cancel the root context;
// funnel_engine.Run drains the JetStream subscription and the NATS
// connection in order before returning nil.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgfunnel "github.com/pericles-luz/crm/internal/adapter/db/postgres/funnel"
	pgfunnelapps "github.com/pericles-luz/crm/internal/adapter/db/postgres/funnelapplications"
	pgfunnelrules "github.com/pericles-luz/crm/internal/adapter/db/postgres/funnelrules"
	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	"github.com/pericles-luz/crm/internal/funnel"
	"github.com/pericles-luz/crm/internal/funnel/engine"
	"github.com/pericles-luz/crm/internal/funnel/rules"
	"github.com/pericles-luz/crm/internal/worker/funnel_engine"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("funnel-engine-worker exited", "err", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := pgpool.NewFromEnv(ctx, os.Getenv)
	if err != nil {
		return fmt.Errorf("pg: connect: %w", err)
	}
	defer pool.Close()

	reg := prometheus.NewRegistry()
	eng, err := assembleEngine(pool, reg, logger)
	if err != nil {
		return err
	}

	sdk, err := natsadapter.Connect(ctx, buildNATSConfig(cfg))
	if err != nil {
		return fmt.Errorf("nats.Connect: %w", err)
	}
	defer sdk.Close()

	// Mount /metrics on a goroutine-managed http server so promscrape
	// can pull the four engine instruments. Shutdown chains to the
	// root context.
	metricsSrv := &http.Server{
		Addr: cfg.metricsAddr,
		Handler: metricsMux(promhttp.HandlerFor(reg, promhttp.HandlerOpts{
			Registry:          reg,
			EnableOpenMetrics: false,
		})),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("funnel-engine-worker: metrics server crashed", "err", err.Error())
		}
	}()
	defer func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		_ = metricsSrv.Shutdown(shutdownCtx)
	}()

	logger.Info("funnel-engine-worker starting",
		"nats", cfg.natsURL,
		"name", cfg.natsName,
		"auth", natsAuthMode(cfg),
		"tls_ca", cfg.natsTLSCAFile,
		"mtls", cfg.natsTLSCertFile != "" && cfg.natsTLSKeyFile != "",
		"insecure", cfg.natsInsecure,
		"metrics_addr", cfg.metricsAddr,
		"ack_wait", cfg.ackWait.String(),
	)

	return Run(ctx, &natsAdapterShim{a: sdk}, funnel_engine.RunConfig{
		Engine:  eng,
		Logger:  logger,
		AckWait: cfg.ackWait,
	})
}

// Run is the testable boundary: tests inject a Subscriber fake plus a
// pre-built RunConfig so the wiring path can be exercised without
// dialing NATS.
func Run(ctx context.Context, sub funnel_engine.Subscriber, cfg funnel_engine.RunConfig) error {
	return runner(ctx, sub, cfg)
}

// runner is an indirection point so wire-up tests can substitute a
// fake for funnel_engine.Run without standing up an embedded NATS
// server (the worker package's own tests already cover the real Run
// against an embedded JetStream). Tests reset runner to the original
// via t.Cleanup.
var runner = funnel_engine.Run

// assembleEngine wires the engine from an already-connected pgxpool
// and a fresh Prometheus registry. Split out of run() so unit tests
// can exercise the adapter-assembly path without dialing NATS, and so
// errors from each stage surface with a stable wrap label.
func assembleEngine(pool *pgxpool.Pool, reg prometheus.Registerer, logger *slog.Logger) (*engine.Engine, error) {
	rulesStore, err := pgfunnelrules.New(pool)
	if err != nil {
		return nil, fmt.Errorf("pg: funnel rules: %w", err)
	}
	appsStore, err := pgfunnelapps.New(pool)
	if err != nil {
		return nil, fmt.Errorf("pg: funnel applications: %w", err)
	}
	funnelStore, err := pgfunnel.New(pool)
	if err != nil {
		return nil, fmt.Errorf("pg: funnel: %w", err)
	}
	resolver, err := rules.NewResolver(rulesStore)
	if err != nil {
		return nil, fmt.Errorf("rules.NewResolver: %w", err)
	}
	funnelService, err := funnel.NewService(funnel.Config{
		Stages:      funnelStore,
		Transitions: funnelStore,
		Publisher:   slogFunnelPublisher{logger: logger},
	})
	if err != nil {
		return nil, fmt.Errorf("funnel.NewService: %w", err)
	}
	metrics := engine.NewMetrics(reg)
	eng, err := engine.NewEngine(engine.Config{
		Resolver:     resolver,
		Applications: appsStore,
		Mover:        funnelService,
		Logger:       logger,
		Metrics:      metrics,
	})
	if err != nil {
		return nil, fmt.Errorf("engine.NewEngine: %w", err)
	}
	return eng, nil
}

// metricsMux exposes the registry on /metrics and a tiny static body
// on / so a probe knows the process is alive.
func metricsMux(metrics http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// slogFunnelPublisher is the funnel.EventPublisher used inside the
// engine worker: every successful MoveConversation emits a structured
// log line. The transition row in Postgres is the source of truth;
// the bus is a notification (matches cmd/server's
// slogFunnelPublisher). When the SIN-62194 message-bus wiring lands
// this gets swapped for a real adapter.
type slogFunnelPublisher struct{ logger *slog.Logger }

func (p slogFunnelPublisher) Publish(_ context.Context, eventName string, payload any) error {
	p.logger.Info("funnel: event published", "event", eventName, "payload", payload)
	return nil
}

// config bundles the parsed env. Required fields stay unexported so
// callers go through loadConfig (which validates).
type config struct {
	natsURL            string
	natsName           string
	natsConnectTimeout time.Duration

	natsToken       string
	natsNKeyFile    string
	natsCredsFile   string
	natsTLSCAFile   string
	natsTLSCertFile string
	natsTLSKeyFile  string
	natsInsecure    bool

	metricsAddr string
	ackWait     time.Duration
}

func loadConfig() (config, error) {
	c := config{
		natsURL:         os.Getenv("NATS_URL"),
		natsName:        envOr("NATS_NAME", "crm-funnel-engine-worker"),
		natsToken:       os.Getenv("NATS_TOKEN"),
		natsNKeyFile:    os.Getenv("NATS_NKEY_FILE"),
		natsCredsFile:   os.Getenv("NATS_CREDS_FILE"),
		natsTLSCAFile:   os.Getenv("NATS_TLS_CA"),
		natsTLSCertFile: os.Getenv("NATS_TLS_CERT"),
		natsTLSKeyFile:  os.Getenv("NATS_TLS_KEY"),
		natsInsecure:    envBool("NATS_INSECURE"),
		metricsAddr:     envOr("FUNNEL_ENGINE_METRICS_ADDR", ":9402"),
	}
	if c.natsURL == "" {
		return c, errors.New("missing required env: NATS_URL")
	}
	if os.Getenv(pgpool.EnvDSN) == "" {
		return c, fmt.Errorf("missing required env: %s", pgpool.EnvDSN)
	}

	if err := validateNATSSecurity(c); err != nil {
		return c, err
	}

	c.natsConnectTimeout = 10 * time.Second
	if v := os.Getenv("NATS_CONNECT_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("NATS_CONNECT_TIMEOUT %q: must be positive Go duration (e.g. 10s)", v)
		}
		c.natsConnectTimeout = d
	}

	c.ackWait = funnel_engine.DefaultAckWait
	if v := os.Getenv("FUNNEL_ENGINE_ACK_WAIT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("FUNNEL_ENGINE_ACK_WAIT %q: must be positive Go duration (e.g. 30s)", v)
		}
		c.ackWait = d
	}

	return c, nil
}

// validateNATSSecurity rejects deploy mistakes at startup. Mirrors
// cmd/wallet-alerter-worker so an operator sees the same wording for
// every Fase 4 worker.
func validateNATSSecurity(c config) error {
	authCount := 0
	if c.natsToken != "" {
		authCount++
	}
	if c.natsNKeyFile != "" {
		authCount++
	}
	if c.natsCredsFile != "" {
		authCount++
	}
	if authCount > 1 {
		return errors.New("NATS auth: set at most one of NATS_TOKEN, NATS_NKEY_FILE, NATS_CREDS_FILE")
	}

	scheme := strings.ToLower(strings.SplitN(c.natsURL, "://", 2)[0])
	switch scheme {
	case "tls", "wss":
		if c.natsTLSCAFile == "" && !c.natsInsecure {
			return fmt.Errorf("NATS_URL is %s:// but NATS_TLS_CA is empty (set NATS_TLS_CA=/path/to/ca.pem or NATS_INSECURE=1 to bypass)", scheme)
		}
	case "nats", "ws":
		if !c.natsInsecure {
			return errors.New("NATS_URL is plaintext; set a tls:// URL with NATS_TLS_CA, or NATS_INSECURE=1 to acknowledge the insecure transport")
		}
	}

	if (c.natsTLSCertFile != "") != (c.natsTLSKeyFile != "") {
		return errors.New("NATS mTLS: NATS_TLS_CERT and NATS_TLS_KEY must be set together")
	}

	return nil
}

func natsAuthMode(c config) string {
	switch {
	case c.natsCredsFile != "":
		return "creds-file"
	case c.natsNKeyFile != "":
		return "nkey-file"
	case c.natsToken != "":
		return "token"
	default:
		return "none"
	}
}

func buildNATSConfig(cfg config) natsadapter.SDKConfig {
	return natsadapter.SDKConfig{
		URL:            cfg.natsURL,
		Name:           cfg.natsName,
		ConnectTimeout: cfg.natsConnectTimeout,
		MaxReconnects:  -1,
		Token:          cfg.natsToken,
		NKeyFile:       cfg.natsNKeyFile,
		CredsFile:      cfg.natsCredsFile,
		TLSCAFile:      cfg.natsTLSCAFile,
		TLSCertFile:    cfg.natsTLSCertFile,
		TLSKeyFile:     cfg.natsTLSKeyFile,
		Insecure:       cfg.natsInsecure,
	}
}

func envBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// natsAdapterShim adapts *natsadapter.SDKAdapter to the worker's
// Subscriber port. Identical pattern to cmd/wallet-alerter-worker —
// duplicated on purpose to keep cmd/funnel-engine-worker free of any
// test-package import.
type natsAdapterShim struct {
	a *natsadapter.SDKAdapter
}

var _ funnel_engine.Subscriber = (*natsAdapterShim)(nil)

func (n *natsAdapterShim) EnsureStream(name string, subjects []string) error {
	return n.a.EnsureStream(name, subjects)
}

func (n *natsAdapterShim) Subscribe(
	ctx context.Context,
	subject, queue, durable string,
	ackWait time.Duration,
	handler funnel_engine.HandlerFunc,
) (funnel_engine.Subscription, error) {
	return n.a.Subscribe(ctx, subject, queue, durable, ackWait,
		func(c context.Context, d *natsadapter.Delivery) error {
			return handler(c, d)
		},
	)
}

func (n *natsAdapterShim) Drain() error { return n.a.Drain() }
