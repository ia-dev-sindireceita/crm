// lgpd-retention-purge-worker is the standalone process described in
// SIN-63186 AC #4: once per Interval (default 1h, configurable via
// LGPD_RETENTION_INTERVAL) it scans lgpd_deletion_request for rows
// whose retention_until is in the past, anonymises the contact's
// remaining personal data, and marks the request completed.
//
// Configuration (12-factor):
//
//	DATABASE_URL                     mandatory — runtime pool DSN.
//	DATABASE_MASTER_OPS_URL          mandatory — app_master_ops DSN. The
//	                                 worker writes across tenants and so
//	                                 must use the master_ops role.
//	LGPD_FISCAL_RETENTION_YEARS      optional, default 5 (Brazilian fiscal
//	                                 baseline). Years to retain fiscal /
//	                                 billing-relevant rows after the
//	                                 erasure request lands.
//	LGPD_RETENTION_INTERVAL          optional Go duration, default 1h.
//	LGPD_RETENTION_BATCH_SIZE        optional integer, default 100.
//	LGPD_RETENTION_ENABLED           optional, default 1. Set to 0 to
//	                                 leave the binary booted but
//	                                 skip the polling loop (rollback).
//
// Shutdown contract: SIGINT and SIGTERM cancel the root context and
// the worker exits cleanly.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	pglgpd "github.com/pericles-luz/crm/internal/adapter/db/postgres/lgpd"
	"github.com/pericles-luz/crm/internal/lgpd"
	"github.com/pericles-luz/crm/internal/worker/lgpd_retention"
)

// config bundles everything loadConfig parses from the environment.
// Extracted so unit tests can drive every env-knob path without
// reaching for pgxpool.
type config struct {
	dsn       string
	masterDSN string
	policy    lgpd.RetentionPolicy
	interval  time.Duration
	batch     int
	enabled   bool
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(rootCtx, logger); err != nil {
		logger.Error("lgpd-retention-purge-worker: exit", "err", err.Error())
		os.Exit(1)
	}
}

// loadConfig parses the environment. Returns a descriptive error so a
// misconfigured deploy surfaces with the exact knob name.
func loadConfig() (config, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return config{}, fmt.Errorf("DATABASE_URL is required")
	}
	masterDSN := os.Getenv("DATABASE_MASTER_OPS_URL")
	if masterDSN == "" {
		return config{}, fmt.Errorf("DATABASE_MASTER_OPS_URL is required")
	}
	years := envInt("LGPD_FISCAL_RETENTION_YEARS", lgpd.DefaultFiscalRetentionYears)
	policy, err := lgpd.NewRetentionPolicy(years)
	if err != nil {
		return config{}, fmt.Errorf("retention policy: %w", err)
	}
	return config{
		dsn:       dsn,
		masterDSN: masterDSN,
		policy:    policy,
		interval:  envDuration("LGPD_RETENTION_INTERVAL", lgpd_retention.DefaultInterval),
		batch:     envInt("LGPD_RETENTION_BATCH_SIZE", lgpd_retention.DefaultBatchSize),
		enabled:   envInt("LGPD_RETENTION_ENABLED", 1) != 0,
	}, nil
}

func run(ctx context.Context, logger *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	runtimePool, err := pgxpool.New(ctx, cfg.dsn)
	if err != nil {
		return fmt.Errorf("pgxpool.New runtime: %w", err)
	}
	defer runtimePool.Close()

	masterPool, err := pgxpool.New(ctx, cfg.masterDSN)
	if err != nil {
		return fmt.Errorf("pgxpool.New master: %w", err)
	}
	defer masterPool.Close()

	store, err := pglgpd.New(runtimePool, masterPool)
	if err != nil {
		return fmt.Errorf("pglgpd.New: %w", err)
	}
	return runWith(ctx, logger, cfg, store, store)
}

// runWith is the testable boundary: production wires the real pgxpool-
// backed Store; tests pass in fakes that satisfy the same ports.
func runWith(
	ctx context.Context,
	logger *slog.Logger,
	cfg config,
	deletions lgpd.DeletionRepository,
	purge lgpd.PurgeRepository,
) error {
	worker, err := lgpd_retention.New(lgpd_retention.Config{
		Deletions: deletions,
		Purge:     purge,
		Logger:    logger,
		Interval:  cfg.interval,
		BatchSize: cfg.batch,
	})
	if err != nil {
		return fmt.Errorf("worker.New: %w", err)
	}
	logger.Info("lgpd-retention-purge-worker: starting",
		"fiscal_retention_years", cfg.policy.FiscalYears,
		"interval", cfg.interval.String(),
		"batch_size", cfg.batch,
		"enabled", cfg.enabled)

	if !cfg.enabled {
		<-ctx.Done()
		logger.Info("lgpd-retention-purge-worker: disabled; shutdown signal received")
		return nil
	}
	return worker.Run(ctx)
}

func envInt(key string, def int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return def
	}
	return d
}
