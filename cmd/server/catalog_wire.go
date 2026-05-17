package main

// SIN-62907 wiring — HTMX catalog admin UI (Fase 3 W4C). Mounts the
// routes under /catalog with the W2B (SIN-62902) postgres adapter
// backing the Product + ProductArgument ports and the W2B cascade
// resolver backing the preview widget.
//
// Two pgxpools are required: the runtime pool (app_runtime, RLS gated)
// serves reads, and the master_ops pool (app_master_ops, audit trigger
// fires) serves writes. When either pool URL is unset or unreachable
// the wire returns a nil handler and the router leaves the /catalog
// routes unmounted — same fail-soft pattern as the other web/* wires.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgcatalog "github.com/pericles-luz/crm/internal/adapter/db/postgres/catalog"
	"github.com/pericles-luz/crm/internal/catalog"
	webcatalog "github.com/pericles-luz/crm/internal/web/catalog"
)

// buildWebCatalogHandler returns the catalog admin mux + a cleanup
// closure that releases the two pgxpools the wire opened. A nil handler
// signals "skip mounting on the public listener" so callers can defer
// the cleanup unconditionally.
func buildWebCatalogHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	noop := func() {}
	dsn := getenv(pgpool.EnvDSN)
	masterDSN := getenv(envMasterOpsDSN)
	if dsn == "" {
		log.Printf("crm: web/catalog disabled — DATABASE_URL unset")
		return nil, noop
	}
	if masterDSN == "" {
		log.Printf("crm: web/catalog disabled — %s unset", envMasterOpsDSN)
		return nil, noop
	}
	runtime, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: web/catalog disabled — pg runtime connect: %v", err)
		return nil, noop
	}
	master, err := pgpool.New(ctx, masterDSN)
	if err != nil {
		runtime.Close()
		log.Printf("crm: web/catalog disabled — pg master_ops connect: %v", err)
		return nil, noop
	}
	store, err := pgcatalog.New(runtime, master)
	if err != nil {
		runtime.Close()
		master.Close()
		log.Printf("crm: web/catalog disabled — catalog store: %v", err)
		return nil, noop
	}
	handler, err := assembleWebCatalogHandler(store, time.Now, slog.Default())
	if err != nil {
		runtime.Close()
		master.Close()
		log.Printf("crm: web/catalog disabled — assemble: %v", err)
		return nil, noop
	}
	log.Printf("crm: web/catalog HTMX routes mounted (catalog adapter + cascade resolver wired)")
	return handler, func() {
		runtime.Close()
		master.Close()
	}
}

// catalogStore unions the W2B ports the wire consumes from the pgx
// adapter. Declared here (composition root) so the test in
// catalog_wire_test.go can swap in an in-memory fake without
// importing pgx.
type catalogStore interface {
	webcatalog.ProductReader
	webcatalog.ProductWriter
	webcatalog.ArgumentReader
	webcatalog.ArgumentWriter
	catalog.ArgumentLister
}

// assembleWebCatalogHandler is the pure assembly seam. Tests call it
// directly with stub deps so the wire is exercised without booting the
// whole server.
func assembleWebCatalogHandler(
	store catalogStore,
	now func() time.Time,
	logger *slog.Logger,
) (http.Handler, error) {
	if store == nil {
		return nil, errors.New("catalog_wire: store is nil")
	}
	if now == nil {
		return nil, errors.New("catalog_wire: now is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	resolver := catalog.NewResolver(store)
	h, err := webcatalog.New(webcatalog.Deps{
		ProductReader:  store,
		ProductWriter:  store,
		ArgumentReader: store,
		ArgumentWriter: store,
		Resolver:       resolver,
		CSRFToken:      csrfTokenFromSessionContext,
		UserID:         userIDFromSessionContext,
		Now:            now,
		Logger:         logger,
	})
	if err != nil {
		return nil, fmt.Errorf("catalog_wire: build handler: %w", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux, nil
}

// Compile-time guard: the pgx adapter satisfies the catalogStore union.
var _ catalogStore = (*pgcatalog.Store)(nil)

// Suppress unused-import warning when building without the pgxpool
// reference (the type is exposed in the wire signature via pgpool).
var _ = (*pgxpool.Pool)(nil)
