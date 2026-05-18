package main

// SIN-62965 / Fase 4 C14 wiring — DunningTick worker.
//
// Mirrors the renewer wire (billing_renewer_wire.go): the worker is
// gated on env so the default boot in CI/dev is unchanged. Required
// env when DUNNING_TICK_ENABLED=1:
//
//   - MASTER_OPS_DATABASE_URL  — DSN that connects as app_master_ops.
//     Shared with the renewer; the wire fails soft if missing.
//   - DUNNING_TICK_ACTOR_ID    — uuid recorded by master_ops_audit so
//     operators can distinguish dunning writes from renewer/operator
//     writes.
//
// Optional knobs:
//
//   - DUNNING_TICK_EVERY      — Go duration, default 1h (matches AC#1).
//   - DUNNING_TICK_BATCH_SIZE — int, default 200.
//   - DUNNING_RUNTIME_DATABASE_URL — DSN that connects as app_runtime,
//     needed for the tenant-scoped CurrentForTenant read path. Defaults
//     to the runtime pool already opened by the public listener
//     (DATABASE_URL); the wire keeps the indirection in case future
//     deploys want a dedicated read DSN.

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
	dunningpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/dunning"
	dunningworker "github.com/pericles-luz/crm/internal/worker/dunning"
)

const (
	envDunningTickEnabled   = "DUNNING_TICK_ENABLED"
	envDunningTickActor     = "DUNNING_TICK_ACTOR_ID"
	envDunningTickEvery     = "DUNNING_TICK_EVERY"
	envDunningTickBatchSize = "DUNNING_TICK_BATCH_SIZE"
	envDunningRuntimeDSN    = "DUNNING_RUNTIME_DATABASE_URL"
)

// dunningTickWiring bundles the artifacts cmd/server needs to drive the
// worker alongside the public listener: a RunWorker callback and a
// Cleanup that releases the pools.
type dunningTickWiring struct {
	RunWorker func(context.Context) error
	Cleanup   func()
}

// dunningTickDial is the test seam — production opens a real pgxpool;
// tests inject a stub that returns a fake pool or nil + error to
// exercise the disabled branch.
type dunningTickDial func(ctx context.Context, dsn string) (*pgxpool.Pool, error)

func defaultDunningTickDial(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return pgpool.New(ctx, dsn)
}

// buildDunningTickWiring constructs the worker wiring or returns nil
// when the feature flag / required env are not set. Fail-soft: a
// misconfigured deploy boots successfully but logs a clear WARN.
func buildDunningTickWiring(ctx context.Context, getenv func(string) string) *dunningTickWiring {
	return buildDunningTickWiringWithDeps(ctx, getenv, defaultDunningTickDial)
}

func buildDunningTickWiringWithDeps(
	ctx context.Context,
	getenv func(string) string,
	dial dunningTickDial,
) *dunningTickWiring {
	if getenv(envDunningTickEnabled) != "1" {
		return nil
	}
	masterDSN := getenv(envMasterOpsDSN)
	if masterDSN == "" {
		log.Printf("crm: dunning tick disabled (%s unset)", envMasterOpsDSN)
		return nil
	}
	runtimeDSN := getenv(envDunningRuntimeDSN)
	if runtimeDSN == "" {
		// Reuse the master DSN: the runtime pool only services the
		// tenant-facing CurrentForTenant read (with WithTenant + RLS).
		// If a deploy wants strict separation it can set the env.
		runtimeDSN = masterDSN
	}
	actorRaw := getenv(envDunningTickActor)
	if actorRaw == "" {
		log.Printf("crm: dunning tick disabled (%s unset)", envDunningTickActor)
		return nil
	}
	actor, err := uuid.Parse(actorRaw)
	if err != nil || actor == uuid.Nil {
		log.Printf("crm: dunning tick disabled — invalid %s=%q: %v", envDunningTickActor, actorRaw, err)
		return nil
	}

	tickEvery := time.Hour
	if v := getenv(envDunningTickEvery); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil || parsed <= 0 {
			log.Printf("crm: dunning tick disabled — invalid %s=%q: %v", envDunningTickEvery, v, err)
			return nil
		}
		tickEvery = parsed
	}

	batchSize := 200
	if v := getenv(envDunningTickBatchSize); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed <= 0 {
			log.Printf("crm: dunning tick disabled — invalid %s=%q: %v", envDunningTickBatchSize, v, err)
			return nil
		}
		batchSize = parsed
	}

	masterPool, err := dial(ctx, masterDSN)
	if err != nil {
		log.Printf("crm: dunning tick disabled — master pg connect: %v", err)
		return nil
	}
	runtimePool, err := dial(ctx, runtimeDSN)
	if err != nil {
		masterPool.Close()
		log.Printf("crm: dunning tick disabled — runtime pg connect: %v", err)
		return nil
	}

	store, err := dunningpg.New(runtimePool, masterPool)
	if err != nil {
		masterPool.Close()
		runtimePool.Close()
		log.Printf("crm: dunning tick disabled — store: %v", err)
		return nil
	}
	tick, err := dunningpg.NewTickStore(store, masterPool)
	if err != nil {
		masterPool.Close()
		runtimePool.Close()
		log.Printf("crm: dunning tick disabled — tick store: %v", err)
		return nil
	}
	courtesy, err := dunningpg.NewCourtesyOverrideStore(masterPool)
	if err != nil {
		masterPool.Close()
		runtimePool.Close()
		log.Printf("crm: dunning tick disabled — courtesy store: %v", err)
		return nil
	}

	metrics := dunningworker.NewMetrics(prometheus.NewRegistry())
	w, err := dunningworker.New(dunningworker.Config{
		Candidates: tick,
		Saver:      tick,
		Courtesy:   courtesy,
		Metrics:    metrics,
		Logger:     slog.Default(),
		ActorID:    actor,
		TickEvery:  tickEvery,
		BatchSize:  batchSize,
	})
	if err != nil {
		masterPool.Close()
		runtimePool.Close()
		log.Printf("crm: dunning tick disabled — worker: %v", err)
		return nil
	}

	runWorker := func(c context.Context) error {
		if err := w.Run(c); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("dunning tick: %w", err)
		}
		return nil
	}
	cleanup := func() {
		masterPool.Close()
		if runtimePool != masterPool {
			runtimePool.Close()
		}
	}
	log.Printf("crm: dunning tick enabled (tick %s, batch %d)", tickEvery, batchSize)
	return &dunningTickWiring{
		RunWorker: runWorker,
		Cleanup:   cleanup,
	}
}
