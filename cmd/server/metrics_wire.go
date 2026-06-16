package main

// SIN-65007 — managerial dashboard metrics read-model wireup (backend
// half of the Dashboard / relatórios epic SIN-64963).
//
// buildMetricsDashboard assembles the read-only metrics stack:
//
//   - pgmetrics.Store — the pgx aggregation adapter (metrics.Reader),
//     tenant-scoped via postgres.WithTenant.
//   - usecase.GetDashboard — resolves the default 30-day window and
//     delegates to the reader.
//
// It deliberately does NOT mount a web route: the dashboard HTMX surface
// is owned by the frontend child of SIN-64963. This wire only exposes the
// use case as a dependency the frontend wireup composes against, mirroring
// the fail-soft pattern of buildWebFunnelHandler (nil + no-op when
// DATABASE_URL is unset so health-only / smoke boots stay clean).

import (
	"context"
	"log"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgmetrics "github.com/pericles-luz/crm/internal/adapter/db/postgres/metrics"
	metricsusecase "github.com/pericles-luz/crm/internal/metrics/usecase"
)

// buildMetricsDashboard returns the dashboard read use case + a cleanup
// closure that releases the pgxpool. A nil use case signals "not wired"
// (DATABASE_URL unset or a connect/build failure) so the frontend wireup
// can skip mounting its route without a panic; the cleanup is always safe
// to defer.
func buildMetricsDashboard(ctx context.Context, getenv func(string) string) (*metricsusecase.GetDashboard, func()) {
	noop := func() {}
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: metrics dashboard disabled (DATABASE_URL unset)")
		return nil, noop
	}
	pool, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: metrics dashboard disabled — pg connect: %v", err)
		return nil, noop
	}
	store, err := pgmetrics.New(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: metrics dashboard disabled — metrics store: %v", err)
		return nil, noop
	}
	uc, err := metricsusecase.NewGetDashboard(store)
	if err != nil {
		pool.Close()
		log.Printf("crm: metrics dashboard disabled — use case: %v", err)
		return nil, noop
	}
	log.Printf("crm: metrics dashboard read-model wired (route owned by frontend child)")
	return uc, func() { pool.Close() }
}
