// wallet-alerter-worker is the standalone process described in
// SIN-62912 (Fase 3 W3D): it subscribes to the `wallet.balance.depleted`
// JetStream subject, formats a human-readable Slack message, and POSTs
// it to the operator alerts channel.
//
// The worker package (internal/worker/wallet_alerter) owns the domain
// logic — decode, dedup, format. This entrypoint only translates the
// environment into ports and hands them to wallet_alerter.Run.
//
// Configuration is read from the environment to keep secrets out of
// flags and config files (12-factor):
//
//	NATS_URL                       mandatory, e.g. tls://nats.example:4222
//	NATS_NAME                      optional, client name surfaced by the
//	                               NATS server. Defaults to
//	                               "crm-wallet-alerter-worker".
//	NATS_CONNECT_TIMEOUT           optional Go duration, default 10s.
//	SLACK_ALERTS_WEBHOOK_URL       optional. Empty = degraded mode: the
//	                               worker boots, consumes + acks events,
//	                               and skips the Slack POST. A warning is
//	                               logged at boot so the operator sees the
//	                               degraded posture (AC #3 of SIN-62905).
//	WALLET_ALERTER_DEDUP_TTL       optional Go duration, default 1h.
//	WALLET_ALERTER_ACK_WAIT        optional Go duration, default 15s.
//
// NATS auth + TLS hardening ([SIN-62815] — same knobs as
// cmd/mediascan-worker). Production deploys MUST set one of these auth
// knobs (pick exactly one):
//
//	NATS_CREDS_FILE     path to chained .creds JWT — preferred for
//	                    production (rotatable on disk, scoped subject).
//	NATS_NKEY_FILE      path to NKey seed file (no JWT).
//	NATS_TOKEN          shared-secret bearer token (legacy / dev).
//
// For TLS / mTLS:
//
//	NATS_TLS_CA         PEM bundle path; required when NATS_URL is
//	                    tls:// or wss:// (unless NATS_INSECURE=1).
//	NATS_TLS_CERT       optional client cert for mTLS (paired w/ KEY).
//	NATS_TLS_KEY        optional client key for mTLS  (paired w/ CERT).
//
// NATS_INSECURE=1 opts the process out of the secure-by-default posture
// (plaintext + no auth). Only acceptable for in-cluster dev rigs that
// ride a private network; pre-deploy review blocks it for prod.
//
// Shutdown contract: SIGINT and SIGTERM cancel the root context.
// wallet_alerter.Run drains the JetStream subscription and the NATS
// connection in order before returning nil — in-flight deliveries get a
// chance to ack before the broker sees the client disconnect.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	slacknotify "github.com/pericles-luz/crm/internal/adapter/notify/slack"
	"github.com/pericles-luz/crm/internal/worker/wallet_alerter"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("wallet-alerter-worker exited", "err", err.Error())
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

	sdk, err := natsadapter.Connect(ctx, buildNATSConfig(cfg))
	if err != nil {
		return fmt.Errorf("nats.Connect: %w", err)
	}
	// wallet_alerter.Run drains on shutdown; Close is the safety net for
	// the error-return path above (Connect succeeded, Run failed) so the
	// fd does not leak.
	defer sdk.Close()

	notifier := slacknotify.New(cfg.slackWebhookURL)

	logger.Info("wallet-alerter-worker starting",
		"nats", cfg.natsURL,
		"name", cfg.natsName,
		"auth", natsAuthMode(cfg),
		"tls_ca", cfg.natsTLSCAFile,
		"mtls", cfg.natsTLSCertFile != "" && cfg.natsTLSKeyFile != "",
		"insecure", cfg.natsInsecure,
		"slack_configured", cfg.slackWebhookURL != "",
		"dedup_ttl", cfg.dedupTTL.String(),
		"ack_wait", cfg.ackWait.String(),
	)

	return Run(ctx, &natsAdapterShim{a: sdk}, walletRunConfig(cfg, notifier, logger))
}

// Run is the testable boundary: tests inject a Subscriber fake plus a
// pre-built RunConfig so the wiring path can be exercised without
// dialling NATS. Production calls it from run() above with the SDK
// adapter shim and the env-driven config.
//
// The thin wrapper lets the test rig assert on the args passed to
// wallet_alerter.Run via a Runner fake; the default Runner is the
// real wallet_alerter.Run function.
func Run(ctx context.Context, sub wallet_alerter.Subscriber, cfg wallet_alerter.RunConfig) error {
	return runner(ctx, sub, cfg)
}

// runner is an indirection point so wire-up tests can substitute a fake
// for wallet_alerter.Run without standing up an embedded NATS server
// (the worker package itself already integration-tests the real Run
// against an embedded JetStream). Tests reset runner to the original
// via t.Cleanup.
var runner = wallet_alerter.Run

// walletRunConfig assembles the wallet_alerter.RunConfig from the
// parsed env and the already-built ports. Pure data transform — no I/O.
func walletRunConfig(cfg config, notifier wallet_alerter.Notifier, logger *slog.Logger) wallet_alerter.RunConfig {
	return wallet_alerter.RunConfig{
		Notifier:       notifier,
		NotifyDegraded: cfg.slackWebhookURL == "",
		Logger:         logger,
		DedupTTL:       cfg.dedupTTL,
		AckWait:        cfg.ackWait,
	}
}

// config is the parsed env. Required fields are kept private to force
// callers through loadConfig (which validates).
type config struct {
	natsURL            string
	natsName           string
	natsConnectTimeout time.Duration

	// NATS auth + TLS — same set as cmd/mediascan-worker. See package
	// doc for the env knobs ([SIN-62815]).
	natsToken       string
	natsNKeyFile    string
	natsCredsFile   string
	natsTLSCAFile   string
	natsTLSCertFile string
	natsTLSKeyFile  string
	natsInsecure    bool

	// Slack #alerts webhook (optional). Empty = degraded mode. The
	// Slack adapter's New("") returns a Notifier whose Notify is a
	// silent no-op (see internal/adapter/notify/slack.New comment), so
	// the worker keeps consuming events without crashing.
	slackWebhookURL string

	// Worker tuning knobs (optional). Defaults match the wallet_alerter
	// package constants.
	dedupTTL time.Duration
	ackWait  time.Duration
}

func loadConfig() (config, error) {
	c := config{
		natsURL:         os.Getenv("NATS_URL"),
		natsName:        envOr("NATS_NAME", "crm-wallet-alerter-worker"),
		natsToken:       os.Getenv("NATS_TOKEN"),
		natsNKeyFile:    os.Getenv("NATS_NKEY_FILE"),
		natsCredsFile:   os.Getenv("NATS_CREDS_FILE"),
		natsTLSCAFile:   os.Getenv("NATS_TLS_CA"),
		natsTLSCertFile: os.Getenv("NATS_TLS_CERT"),
		natsTLSKeyFile:  os.Getenv("NATS_TLS_KEY"),
		natsInsecure:    envBool("NATS_INSECURE"),
		slackWebhookURL: os.Getenv("SLACK_ALERTS_WEBHOOK_URL"),
	}
	if c.natsURL == "" {
		return c, errors.New("missing required env: NATS_URL")
	}

	// Cross-field NATS security validation. SDKConfig.validate() would
	// catch most of this on Connect, but failing earlier with a config-
	// level message makes the deploy error clearer to the operator.
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

	c.dedupTTL = wallet_alerter.DefaultDedupTTL
	if v := os.Getenv("WALLET_ALERTER_DEDUP_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("WALLET_ALERTER_DEDUP_TTL %q: must be positive Go duration (e.g. 1h)", v)
		}
		c.dedupTTL = d
	}

	c.ackWait = wallet_alerter.DefaultAckWait
	if v := os.Getenv("WALLET_ALERTER_ACK_WAIT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("WALLET_ALERTER_ACK_WAIT %q: must be positive Go duration (e.g. 15s)", v)
		}
		c.ackWait = d
	}

	return c, nil
}

// validateNATSSecurity rejects deploy mistakes at startup before any
// socket is opened. Mirrors cmd/mediascan-worker so an operator sees
// the same wording for both workers.
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

// natsAuthMode reports which auth knob is wired, never the secret
// itself. Used for the startup log line.
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

// buildNATSConfig translates the parsed env into the SDK-shaped
// SDKConfig the natsadapter consumes. Pure data transform — no I/O —
// so it can be exercised by unit tests without dialing NATS.
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
// Subscriber port. Identical pattern to the integration-test shim at
// internal/worker/wallet_alerter/integration_test.go and the
// cmd/mediascan-worker shim — duplicated here on purpose to keep
// cmd/wallet-alerter-worker free of any test-package import.
type natsAdapterShim struct {
	a *natsadapter.SDKAdapter
}

// Compile-time fence: *natsAdapterShim must satisfy wallet_alerter.Subscriber.
var _ wallet_alerter.Subscriber = (*natsAdapterShim)(nil)

func (n *natsAdapterShim) EnsureStream(name string, subjects []string) error {
	return n.a.EnsureStream(name, subjects)
}

func (n *natsAdapterShim) Subscribe(
	ctx context.Context,
	subject, queue, durable string,
	ackWait time.Duration,
	handler wallet_alerter.HandlerFunc,
) (wallet_alerter.Subscription, error) {
	return n.a.Subscribe(ctx, subject, queue, durable, ackWait,
		func(c context.Context, d *natsadapter.Delivery) error {
			return handler(c, d)
		},
	)
}

func (n *natsAdapterShim) Drain() error { return n.a.Drain() }
