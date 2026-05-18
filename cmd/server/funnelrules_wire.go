package main

// SIN-62961 wiring — HTMX funnel-rules editor (Fase 4). Mounts the
// routes under /funnel/rules. The SIN-62955 pgx adapter satisfies
// both the read port (ListEffectiveForChannel) and the admin port
// (Create/Get/Update/SetEnabled/Delete/ListAll) — one runtime pool is
// enough.
//
// Returns (nil, no-op) when DATABASE_URL is unset so cmd/server keeps
// booting cleanly in health-only / smoke modes (same fail-soft pattern
// as the funnel / catalog / campaigns wires).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"time"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgfunnelrules "github.com/pericles-luz/crm/internal/adapter/db/postgres/funnelrules"
	domain "github.com/pericles-luz/crm/internal/funnel/rules"
	webfunnelrules "github.com/pericles-luz/crm/internal/web/funnel/rules"
)

// buildWebFunnelRulesHandler returns the editor mux + a cleanup
// closure that releases the pgxpool.
func buildWebFunnelRulesHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	noop := func() {}
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: web/funnel/rules disabled — DATABASE_URL unset")
		return nil, noop
	}
	pool, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: web/funnel/rules disabled — pg connect: %v", err)
		return nil, noop
	}
	store, err := pgfunnelrules.New(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/funnel/rules disabled — store: %v", err)
		return nil, noop
	}
	handler, err := assembleWebFunnelRulesHandler(store, time.Now, slog.Default())
	if err != nil {
		pool.Close()
		log.Printf("crm: web/funnel/rules disabled — assemble: %v", err)
		return nil, noop
	}
	log.Printf("crm: web/funnel/rules HTMX routes mounted on public listener")
	return handler, func() { pool.Close() }
}

// funnelRulesStore unions the two domain ports the web handler reaches
// for via its repo + resolver dependencies. The pgx adapter satisfies
// both; declaring the union here (composition root) keeps the test in
// funnelrules_wire_test.go free of pgx imports.
type funnelRulesStore interface {
	domain.RuleAdminRepository
	domain.RuleRepository
}

// assembleWebFunnelRulesHandler is the pure assembly seam. Tests call
// it with an in-memory fake to exercise the wire without booting
// Postgres.
func assembleWebFunnelRulesHandler(
	store funnelRulesStore,
	now func() time.Time,
	logger *slog.Logger,
) (http.Handler, error) {
	if store == nil {
		return nil, errors.New("funnelrules_wire: store is nil")
	}
	if now == nil {
		return nil, errors.New("funnelrules_wire: now is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	resolver, err := domain.NewResolver(store)
	if err != nil {
		return nil, fmt.Errorf("funnelrules_wire: build resolver: %w", err)
	}
	h, err := webfunnelrules.New(webfunnelrules.Deps{
		Repo:      store,
		Resolver:  resolver,
		CSRFToken: csrfTokenFromSessionContext,
		UserID:    userIDFromSessionContext,
		Now:       func() time.Time { return now().UTC() },
		Logger:    logger,
	})
	if err != nil {
		return nil, fmt.Errorf("funnelrules_wire: build handler: %w", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux, nil
}

// Compile-time guard: the pgx adapter satisfies the funnelRulesStore
// union the wire requires.
var _ funnelRulesStore = (*pgfunnelrules.Store)(nil)
