package main

// SIN-62879 / Fase 2.5 C5 wiring — BillingRenewer worker.
//
// Mounted alongside the existing webhook reconciler (cmd/server/main.go),
// the renewer sweeps active subscriptions whose current_period_end has
// elapsed, advances them by one month, inserts a pending invoice for
// the new period, and publishes a subscription.renewed event on
// JetStream. The flow is gated on three env vars so the default boot
// in CI/dev is unchanged:
//
//   - BILLING_RENEWER_ENABLED=1   — feature flag (default off).
//   - MASTER_OPS_DATABASE_URL     — DSN that connects as app_master_ops.
//     Required when enabled: WithMasterOps refuses to run under the
//     runtime role and the audit trigger refuses to write without it.
//   - BILLING_RENEWER_ACTOR_ID    — uuid recorded by master_ops_audit
//     so operators can distinguish renewer writes from human writes.
//
// Optional knobs:
//   - BILLING_RENEWER_TICK_EVERY  — Go duration string, default 1h.
//   - BILLING_RENEWER_BATCH_SIZE  — int, default 100.
//
// The NATS publisher is taken from the JetStream wire if it's running;
// otherwise a no-op publisher logs a startup WARN. Production deploys
// MUST attach a real publisher before flipping BILLING_RENEWER_ENABLED.
// (Hooking a process-wide JetStream conn into this wire is a follow-up;
// the noop-fallback keeps cmd/server testable.)

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
	"github.com/prometheus/client_golang/prometheus"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	billingpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/billing"
	billingworker "github.com/pericles-luz/crm/internal/worker/billing"
)

const (
	envBillingRenewerEnabled   = "BILLING_RENEWER_ENABLED"
	envBillingRenewerActor     = "BILLING_RENEWER_ACTOR_ID"
	envBillingRenewerTickEvery = "BILLING_RENEWER_TICK_EVERY"
	envBillingRenewerBatchSize = "BILLING_RENEWER_BATCH_SIZE"
	envMasterOpsDSN            = "MASTER_OPS_DATABASE_URL"
)

// billingRenewerWiring bundles the artifacts cmd/server needs to drive
// the renewer alongside the public listener: a RunWorker callback and a
// Cleanup that releases the master_ops pool.
type billingRenewerWiring struct {
	RunWorker func(context.Context) error
	Cleanup   func()
}

// billingRenewerDial is the test seam — production opens a real pgxpool
// against MASTER_OPS_DATABASE_URL; tests inject a stub that returns a
// fake pool (or nil + error to exercise the disabled branch).
type billingRenewerDial func(ctx context.Context, dsn string) (*pgxpool.Pool, error)

func defaultBillingRenewerDial(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return pgpool.New(ctx, dsn)
}

// buildBillingRenewerWiring constructs the renewer wiring or returns
// nil when the feature flag / required env are not set. The flow is
// fail-soft so a misconfigured deploy boots successfully but logs a
// clear WARN.
func buildBillingRenewerWiring(ctx context.Context, getenv func(string) string) *billingRenewerWiring {
	return buildBillingRenewerWiringWithDeps(ctx, getenv, defaultBillingRenewerDial)
}

func buildBillingRenewerWiringWithDeps(
	ctx context.Context,
	getenv func(string) string,
	dial billingRenewerDial,
) *billingRenewerWiring {
	if getenv(envBillingRenewerEnabled) != "1" {
		return nil
	}
	dsn := getenv(envMasterOpsDSN)
	if dsn == "" {
		log.Printf("crm: billing renewer disabled (%s unset)", envMasterOpsDSN)
		return nil
	}
	actorRaw := getenv(envBillingRenewerActor)
	if actorRaw == "" {
		log.Printf("crm: billing renewer disabled (%s unset)", envBillingRenewerActor)
		return nil
	}
	actor, err := uuid.Parse(actorRaw)
	if err != nil || actor == uuid.Nil {
		log.Printf("crm: billing renewer disabled — invalid %s=%q: %v", envBillingRenewerActor, actorRaw, err)
		return nil
	}

	tickEvery := time.Hour
	if v := getenv(envBillingRenewerTickEvery); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil || parsed <= 0 {
			log.Printf("crm: billing renewer disabled — invalid %s=%q: %v", envBillingRenewerTickEvery, v, err)
			return nil
		}
		tickEvery = parsed
	}

	batchSize := 100
	if v := getenv(envBillingRenewerBatchSize); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed <= 0 {
			log.Printf("crm: billing renewer disabled — invalid %s=%q: %v", envBillingRenewerBatchSize, v, err)
			return nil
		}
		batchSize = parsed
	}

	pool, err := dial(ctx, dsn)
	if err != nil {
		log.Printf("crm: billing renewer disabled — pg connect: %v", err)
		return nil
	}

	store, err := billingpg.NewRenewerStore(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: billing renewer disabled — store: %v", err)
		return nil
	}

	publisher := newBillingNoopPublisher()
	metrics := billingworker.NewMetrics(prometheus.NewRegistry())

	r, err := billingworker.New(billingworker.Config{
		Due:       store,
		Renewer:   store,
		Publisher: publisher,
		Metrics:   metrics,
		Logger:    slog.Default(),
		ActorID:   actor,
		TickEvery: tickEvery,
		BatchSize: batchSize,
	})
	if err != nil {
		pool.Close()
		log.Printf("crm: billing renewer disabled — worker: %v", err)
		return nil
	}

	runWorker := func(c context.Context) error {
		if err := r.Run(c); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("billing renewer: %w", err)
		}
		return nil
	}
	cleanup := func() { pool.Close() }
	log.Printf("crm: billing renewer enabled (tick %s, batch %d)", tickEvery, batchSize)
	return &billingRenewerWiring{
		RunWorker: runWorker,
		Cleanup:   cleanup,
	}
}

// billingNoopPublisher swallows publish requests until a real
// JetStream publisher replaces it. Production deploys MUST swap before
// flipping BILLING_RENEWER_ENABLED=1; the WARN flags the gap in logs.
type billingNoopPublisher struct{}

func newBillingNoopPublisher() *billingNoopPublisher {
	slog.Default().Warn(
		"billing renewer: publisher running in no-op mode; subscription.renewed events are NOT delivered. Swap to a JetStream-backed publisher before flipping BILLING_RENEWER_ENABLED=1 in production.",
		slog.String("component", "billing.renewer.publisher"),
	)
	return &billingNoopPublisher{}
}

func (*billingNoopPublisher) Publish(context.Context, string, string, []byte) error {
	return nil
}

// Compile-time guard.
var _ billingworker.EventPublisher = (*billingNoopPublisher)(nil)
