package main

// SIN-62962 wiring — HTMX marketing-campaign dashboard. Mounts the
// routes under /campaigns. The SIN-62954 pgx adapter satisfies every
// port the web handler depends on; one runtime pool is enough.
//
// Returns (nil, no-op) when DATABASE_URL is unset so cmd/server keeps
// booting cleanly in health-only / smoke modes (same fail-soft pattern
// as the funnel / catalog wires).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"time"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgcampaigns "github.com/pericles-luz/crm/internal/adapter/db/postgres/campaigns"
	webcampaigns "github.com/pericles-luz/crm/internal/web/campaigns"
)

// buildWebCampaignsHandler returns the dashboard mux + a cleanup
// closure that releases the pgxpool.
func buildWebCampaignsHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	noop := func() {}
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: web/campaigns disabled — DATABASE_URL unset")
		return nil, noop
	}
	pool, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: web/campaigns disabled — pg connect: %v", err)
		return nil, noop
	}
	store, err := pgcampaigns.New(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/campaigns disabled — store: %v", err)
		return nil, noop
	}
	handler, err := assembleWebCampaignsHandler(store, time.Now, slog.Default())
	if err != nil {
		pool.Close()
		log.Printf("crm: web/campaigns disabled — assemble: %v", err)
		return nil, noop
	}
	log.Printf("crm: web/campaigns HTMX routes mounted on public listener")
	return handler, func() { pool.Close() }
}

// campaignsStore unions the four ports the web handler consumes. The
// pgx adapter satisfies all four; declaring the union here (composition
// root) keeps the test in campaigns_wire_test.go free of pgx imports.
type campaignsStore interface {
	webcampaigns.CampaignReader
	webcampaigns.CampaignWriter
	webcampaigns.CampaignStatsReader
	webcampaigns.CampaignClickLister
}

// assembleWebCampaignsHandler is the pure assembly seam. Tests call it
// with an in-memory fake to exercise the wire without booting Postgres.
func assembleWebCampaignsHandler(
	store campaignsStore,
	now func() time.Time,
	logger *slog.Logger,
) (http.Handler, error) {
	if store == nil {
		return nil, errors.New("campaigns_wire: store is nil")
	}
	if now == nil {
		return nil, errors.New("campaigns_wire: now is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	h, err := webcampaigns.New(webcampaigns.Deps{
		Reader:    store,
		Writer:    store,
		Stats:     store,
		Clicks:    store,
		CSRFToken: csrfTokenFromSessionContext,
		UserID:    userIDFromSessionContext,
		Now:       func() time.Time { return now().UTC() },
		Logger:    logger,
	})
	if err != nil {
		return nil, fmt.Errorf("campaigns_wire: build handler: %w", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux, nil
}

// Compile-time guard: the pgx adapter satisfies the campaignsStore
// union the wire requires.
var _ campaignsStore = (*pgcampaigns.Store)(nil)
