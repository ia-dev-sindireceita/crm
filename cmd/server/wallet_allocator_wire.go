package main

// SIN-62881 / Fase 2.5 C6 wiring — WalletAllocator worker.
//
// Mounted alongside the BillingRenewer (SIN-62879) in cmd/server/main.go,
// the allocator subscribes to subscription.renewed JetStream messages,
// looks up the plan's monthly_token_quota, and credits the tenant
// wallet idempotently via wallet.MonthlyAllocator. Gated on env so the
// default boot in CI/dev is unchanged:
//
//   - WALLET_ALLOCATOR_ENABLED=1   — feature flag (default off).
//   - DATABASE_URL                 — runtime DSN (for plan lookups).
//   - MASTER_OPS_DATABASE_URL      — master_ops DSN (for wallet writes).
//   - WALLET_ALLOCATOR_ACTOR_ID    — uuid recorded by master_ops_audit
//     so operators can distinguish allocator writes from human writes.
//   - NATS_URL                     — JetStream endpoint (TLS strongly
//     preferred; plaintext requires NATS_INSECURE=1).
//
// Optional knobs:
//   - WALLET_ALLOCATOR_DURABLE     — durable consumer name (default
//     "wallet-allocator").
//   - WALLET_ALLOCATOR_STREAM      — stream binding (default empty;
//     JetStream auto-binds when the subject is unique to one stream).
//   - WALLET_ALLOCATOR_ACK_WAIT    — per-message ack deadline
//     (default 30s).
//   - WALLET_ALLOCATOR_MAX_DELIVER — per-message max delivery attempts
//     (default 6; must be > len of default BackOff which is 5).
//
// Auth/TLS knobs share the NATS_* prefix used by cmd/mediascan-worker:
//   - NATS_TOKEN, NATS_NKEY_FILE, NATS_CREDS_FILE,
//   - NATS_TLS_CA, NATS_TLS_CERT, NATS_TLS_KEY,
//   - NATS_INSECURE=1 (explicit opt-out for plaintext/anonymous).
//
// Fail-soft: any misconfigured env disables the wire-up with a clear
// WARN; the server keeps booting. Production deploys MUST set the
// required envs before flipping WALLET_ALLOCATOR_ENABLED.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	natsgo "github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	billingpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/billing"
	walletpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/wallet"
	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	walletworker "github.com/pericles-luz/crm/internal/worker/wallet"
)

const (
	envWalletAllocatorEnabled    = "WALLET_ALLOCATOR_ENABLED"
	envWalletAllocatorActor      = "WALLET_ALLOCATOR_ACTOR_ID"
	envWalletAllocatorDurable    = "WALLET_ALLOCATOR_DURABLE"
	envWalletAllocatorStream     = "WALLET_ALLOCATOR_STREAM"
	envWalletAllocatorAckWait    = "WALLET_ALLOCATOR_ACK_WAIT"
	envWalletAllocatorMaxDeliver = "WALLET_ALLOCATOR_MAX_DELIVER"
	envRuntimeDSN                = "DATABASE_URL"
	envNATSURL                   = "NATS_URL"
	envNATSToken                 = "NATS_TOKEN"
	envNATSNKey                  = "NATS_NKEY_FILE"
	envNATSCreds                 = "NATS_CREDS_FILE"
	envNATSTLSCA                 = "NATS_TLS_CA"
	envNATSTLSCert               = "NATS_TLS_CERT"
	envNATSTLSKey                = "NATS_TLS_KEY"
	envNATSInsecure              = "NATS_INSECURE"
)

// walletAllocatorWiring bundles the artifacts cmd/server needs to drive
// the allocator alongside the public listener.
type walletAllocatorWiring struct {
	RunWorker func(context.Context) error
	Cleanup   func()
}

// walletAllocatorDial is the test seam for the Postgres pools. Returns
// (runtimePool, masterPool, err).
type walletAllocatorDial func(ctx context.Context, runtimeDSN, masterDSN string) (*pgxpool.Pool, *pgxpool.Pool, error)

// walletAllocatorNATSConnect is the test seam for the JetStream
// context. Returns (jsContext, drainFn, err); drainFn is called by
// Cleanup so in-flight messages have a chance to ack.
type walletAllocatorNATSConnect func(ctx context.Context, cfg natsadapter.SDKConfig) (natsgo.JetStreamContext, func(), error)

func defaultWalletAllocatorDial(ctx context.Context, runtimeDSN, masterDSN string) (*pgxpool.Pool, *pgxpool.Pool, error) {
	rp, err := pgpool.New(ctx, runtimeDSN)
	if err != nil {
		return nil, nil, fmt.Errorf("runtime pool: %w", err)
	}
	mp, err := pgpool.New(ctx, masterDSN)
	if err != nil {
		rp.Close()
		return nil, nil, fmt.Errorf("master pool: %w", err)
	}
	return rp, mp, nil
}

func defaultWalletAllocatorNATSConnect(ctx context.Context, cfg natsadapter.SDKConfig) (natsgo.JetStreamContext, func(), error) {
	a, err := natsadapter.Connect(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	// natsadapter.SDKAdapter exposes Drain but not the raw JS context.
	// Reach the JetStream context via a fresh nats.go connection here:
	// the SDKAdapter is intentionally narrow for the mediascan worker.
	// For the allocator we want the JS surface plus the same Connect
	// security posture, so we redial the conn for now and accept the
	// extra round-trip at process start.
	//
	// TODO follow-up: extend SDKAdapter with a JS() accessor so we can
	// reuse a single conn here.
	_ = a // keep import alive; future refactor
	conn, err := natsgo.Connect(cfg.URL, walletAllocatorNATSOptions(cfg)...)
	if err != nil {
		a.Close()
		return nil, nil, fmt.Errorf("nats: redial for JS: %w", err)
	}
	js, err := conn.JetStream()
	if err != nil {
		conn.Close()
		a.Close()
		return nil, nil, fmt.Errorf("nats: jetstream context: %w", err)
	}
	drain := func() {
		_ = conn.Drain()
		_ = a.Drain()
	}
	return js, drain, nil
}

// walletAllocatorNATSOptions translates SDKConfig into the nats.go
// options needed for the secondary JS-only connection. Auth + TLS
// fields are honored; secure-by-default validation already ran on
// the SDKAdapter, so we trust the values here.
func walletAllocatorNATSOptions(cfg natsadapter.SDKConfig) []natsgo.Option {
	opts := []natsgo.Option{
		natsgo.Name("crm-wallet-allocator"),
		natsgo.Timeout(10 * time.Second),
		natsgo.MaxReconnects(-1),
	}
	switch {
	case cfg.CredsFile != "":
		opts = append(opts, natsgo.UserCredentials(cfg.CredsFile))
	case cfg.NKeyFile != "":
		if nkeyOpt, err := natsgo.NkeyOptionFromSeed(cfg.NKeyFile); err == nil {
			opts = append(opts, nkeyOpt)
		}
	case cfg.Token != "":
		opts = append(opts, natsgo.Token(cfg.Token))
	}
	if cfg.TLSCAFile != "" {
		opts = append(opts, natsgo.RootCAs(cfg.TLSCAFile))
	}
	if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
		opts = append(opts, natsgo.ClientCert(cfg.TLSCertFile, cfg.TLSKeyFile))
	}
	return opts
}

// buildWalletAllocatorWiring constructs the allocator wiring or returns
// nil when the feature flag / required env are not set.
func buildWalletAllocatorWiring(ctx context.Context, getenv func(string) string) *walletAllocatorWiring {
	return buildWalletAllocatorWiringWithDeps(ctx, getenv, defaultWalletAllocatorDial, defaultWalletAllocatorNATSConnect)
}

func buildWalletAllocatorWiringWithDeps(
	ctx context.Context,
	getenv func(string) string,
	dial walletAllocatorDial,
	nc walletAllocatorNATSConnect,
) *walletAllocatorWiring {
	if getenv(envWalletAllocatorEnabled) != "1" {
		return nil
	}

	runtimeDSN := getenv(envRuntimeDSN)
	if runtimeDSN == "" {
		log.Printf("crm: wallet allocator disabled (%s unset)", envRuntimeDSN)
		return nil
	}
	masterDSN := getenv("MASTER_OPS_DATABASE_URL")
	if masterDSN == "" {
		log.Printf("crm: wallet allocator disabled (MASTER_OPS_DATABASE_URL unset)")
		return nil
	}
	actorRaw := getenv(envWalletAllocatorActor)
	if actorRaw == "" {
		log.Printf("crm: wallet allocator disabled (%s unset)", envWalletAllocatorActor)
		return nil
	}
	actor, err := uuid.Parse(actorRaw)
	if err != nil || actor == uuid.Nil {
		log.Printf("crm: wallet allocator disabled — invalid %s=%q: %v", envWalletAllocatorActor, actorRaw, err)
		return nil
	}
	natsURL := getenv(envNATSURL)
	if natsURL == "" {
		log.Printf("crm: wallet allocator disabled (%s unset)", envNATSURL)
		return nil
	}

	ackWait := 30 * time.Second
	if v := getenv(envWalletAllocatorAckWait); v != "" {
		parsed, perr := time.ParseDuration(v)
		if perr != nil || parsed <= 0 {
			log.Printf("crm: wallet allocator disabled — invalid %s=%q: %v", envWalletAllocatorAckWait, v, perr)
			return nil
		}
		ackWait = parsed
	}

	maxDeliver := 6
	if v := getenv(envWalletAllocatorMaxDeliver); v != "" {
		parsed, perr := strconv.Atoi(v)
		if perr != nil || parsed <= 1 {
			log.Printf("crm: wallet allocator disabled — invalid %s=%q: %v", envWalletAllocatorMaxDeliver, v, perr)
			return nil
		}
		maxDeliver = parsed
	}

	runtimePool, masterPool, err := dial(ctx, runtimeDSN, masterDSN)
	if err != nil {
		log.Printf("crm: wallet allocator disabled — pg connect: %v", err)
		return nil
	}

	plans, err := billingpg.New(runtimePool, masterPool)
	if err != nil {
		runtimePool.Close()
		masterPool.Close()
		log.Printf("crm: wallet allocator disabled — billing store: %v", err)
		return nil
	}
	alloc, err := walletpg.NewMonthlyAllocatorStore(masterPool, actor)
	if err != nil {
		runtimePool.Close()
		masterPool.Close()
		log.Printf("crm: wallet allocator disabled — wallet allocator store: %v", err)
		return nil
	}

	natsCfg := natsadapter.SDKConfig{
		URL:         natsURL,
		Name:        "crm-wallet-allocator",
		Token:       getenv(envNATSToken),
		NKeyFile:    getenv(envNATSNKey),
		CredsFile:   getenv(envNATSCreds),
		TLSCAFile:   getenv(envNATSTLSCA),
		TLSCertFile: getenv(envNATSTLSCert),
		TLSKeyFile:  getenv(envNATSTLSKey),
		Insecure:    truthyEnv(getenv(envNATSInsecure)),
	}
	js, drainNATS, err := nc(ctx, natsCfg)
	if err != nil {
		runtimePool.Close()
		masterPool.Close()
		log.Printf("crm: wallet allocator disabled — nats connect: %v", err)
		return nil
	}

	sub, err := natsadapter.NewWalletSubscriber(natsadapter.WalletSubscriberConfig{
		JS:         js,
		Subject:    walletworker.SubjectSubscriptionRenewed,
		Durable:    envOr(getenv, envWalletAllocatorDurable, "wallet-allocator"),
		Stream:     getenv(envWalletAllocatorStream),
		AckWait:    ackWait,
		MaxDeliver: maxDeliver,
	})
	if err != nil {
		drainNATS()
		runtimePool.Close()
		masterPool.Close()
		log.Printf("crm: wallet allocator disabled — subscriber: %v", err)
		return nil
	}

	metrics := walletworker.NewMetrics(prometheus.NewRegistry())
	a, err := walletworker.New(walletworker.Config{
		Subscriber: sub,
		Plans:      plans,
		Allocator:  alloc,
		Logger:     slog.Default(),
		Metrics:    metrics,
	})
	if err != nil {
		drainNATS()
		runtimePool.Close()
		masterPool.Close()
		log.Printf("crm: wallet allocator disabled — worker: %v", err)
		return nil
	}

	runWorker := func(c context.Context) error {
		if err := a.Run(c); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("wallet allocator: %w", err)
		}
		return nil
	}
	cleanup := func() {
		drainNATS()
		runtimePool.Close()
		masterPool.Close()
	}
	log.Printf("crm: wallet allocator enabled (ack_wait %s, max_deliver %d)", ackWait, maxDeliver)
	return &walletAllocatorWiring{
		RunWorker: runWorker,
		Cleanup:   cleanup,
	}
}

func truthyEnv(v string) bool {
	switch v {
	case "1", "true", "TRUE", "True", "yes", "on":
		return true
	}
	return false
}

func envOr(getenv func(string) string, key, fallback string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}
