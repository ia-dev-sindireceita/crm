package main

// SIN-62906 wiring — HTMX AI policy admin UI (Fase 3 W4A). Mounts
// the routes under /settings/ai-policy with the cascade resolver
// (SIN-62351 / W2A) backing both the per-row CRUD and the preview
// widget.
//
// Audit trail (SIN-62353 / H1) is out of scope for this wave: the
// Repository the handler sees is the bare pgx adapter, not the
// RecordingRepository decorator. H1 will swap the decorator in
// without changing the handler — the seam is the aipolicy.Repository
// interface.
//
// When DATABASE_URL is unset or the pgx pool / aipolicy store cannot
// be built, the wire returns a nil handler and the router leaves the
// routes unmounted. This mirrors WebContacts / WebFunnel; the page
// is not LGPD-blocking (privacy disclosure is) so degrading-to-404
// is acceptable.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"time"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgaipolicy "github.com/pericles-luz/crm/internal/adapter/db/postgres/aipolicy"
	"github.com/pericles-luz/crm/internal/aipolicy"
	webaipolicy "github.com/pericles-luz/crm/internal/web/aipolicy"
)

// buildWebAIPolicyHandler returns the admin UI mux + cleanup. A nil
// handler means "skip the mount"; the router treats that as "don't
// expose the routes" which is the safe default when the DB is
// unreachable.
func buildWebAIPolicyHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	noop := func() {}
	if dsn := getenv(pgpool.EnvDSN); dsn == "" {
		log.Printf("crm: web/aipolicy disabled — DATABASE_URL unset")
		return nil, noop
	}
	pool, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: web/aipolicy disabled — pg connect: %v", err)
		return nil, noop
	}
	store, err := pgaipolicy.New(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/aipolicy disabled — aipolicy store: %v", err)
		return nil, noop
	}
	resolver, err := aipolicy.NewResolver(store)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/aipolicy disabled — aipolicy resolver: %v", err)
		return nil, noop
	}
	handler, err := assembleWebAIPolicyHandler(store, resolver, time.Now, slog.Default())
	if err != nil {
		pool.Close()
		log.Printf("crm: web/aipolicy disabled — assemble: %v", err)
		return nil, noop
	}
	log.Printf("crm: web/aipolicy HTMX routes mounted (cascade resolver wired)")
	return handler, func() { pool.Close() }
}

// assembleWebAIPolicyHandler is the pure assembly seam. Tests call
// it directly with stub deps so the wire is exercised without booting
// the whole server.
func assembleWebAIPolicyHandler(
	repo aipolicy.Repository,
	resolver webaipolicy.Resolver,
	now func() time.Time,
	logger *slog.Logger,
) (http.Handler, error) {
	if repo == nil {
		return nil, errors.New("ai_policy_wire: repo is nil")
	}
	if resolver == nil {
		return nil, errors.New("ai_policy_wire: resolver is nil")
	}
	if now == nil {
		return nil, errors.New("ai_policy_wire: now is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	h, err := webaipolicy.New(webaipolicy.Deps{
		Repo:     repo,
		Resolver: resolver,
		Now:      now,
		Logger:   logger,
	})
	if err != nil {
		return nil, fmt.Errorf("ai_policy_wire: build handler: %w", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux, nil
}
