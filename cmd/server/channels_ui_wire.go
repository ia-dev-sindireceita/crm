package main

// SIN-66391 (P2) wiring — HTMX channel-management admin UI. Mounts the
// routes under /settings/channels backed by the SIN-66389 channels
// Postgres adapter (channels.Repository + channels.AccessRepository).
//
// Only the runtime pool (app_runtime, RLS-gated) is needed: the
// channel_access master-ops audit trigger is a no-op for app_runtime
// (current_user <> 'app_master_ops' → RETURN NEW), so the roster writes
// route through the same pool as the reads. When DATABASE_URL is unset or
// unreachable the wire returns a nil handler and the router leaves the
// /settings/channels routes unmounted — the same fail-soft pattern as the
// other web/* wires.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgchannels "github.com/pericles-luz/crm/internal/adapter/db/postgres/channels"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	"github.com/pericles-luz/crm/internal/channels"
	webchannels "github.com/pericles-luz/crm/internal/web/channels"
	"github.com/pericles-luz/crm/internal/web/userlabel"
)

// buildWebChannelsHandler returns the channel-management admin mux + a
// cleanup closure that releases the pgxpool the wire opened. A nil handler
// signals "skip mounting on the public listener" so callers can defer the
// cleanup unconditionally.
func buildWebChannelsHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	noop := func() {}
	if getenv(pgpool.EnvDSN) == "" {
		log.Printf("crm: web/channels disabled — DATABASE_URL unset")
		return nil, noop
	}
	pool, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: web/channels disabled — pg runtime connect: %v", err)
		return nil, noop
	}
	store, err := pgchannels.New(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/channels disabled — channels store: %v", err)
		return nil, noop
	}
	// The user-label directory is best-effort chrome: a failure degrades
	// the app-shell account label to the shell fallback ("Conta"), it does
	// not disable the surface.
	var userDir userlabel.Directory
	if dir, derr := pginbox.NewUserDirectory(pool); derr != nil {
		log.Printf("crm: web/channels — user directory unavailable, using shell fallback: %v", derr)
	} else {
		userDir = dir
	}
	handler, err := assembleWebChannelsHandler(store, userDir, slog.Default())
	if err != nil {
		pool.Close()
		log.Printf("crm: web/channels disabled — assemble: %v", err)
		return nil, noop
	}
	log.Printf("crm: web/channels HTMX routes mounted (channels adapter wired)")
	return handler, func() { pool.Close() }
}

// channelsStore unions the ports the wire consumes from the pgx adapter.
// Declared at the composition root so the test can swap in an in-memory
// fake without importing pgx.
type channelsStore interface {
	channels.Repository
	channels.AccessRepository
}

// assembleWebChannelsHandler is the pure assembly seam. Tests call it
// directly with a stub store so the wire is exercised without booting the
// whole server.
func assembleWebChannelsHandler(store channelsStore, userLabels userlabel.Directory, logger *slog.Logger) (http.Handler, error) {
	if store == nil {
		return nil, errors.New("channels_wire: store is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	h, err := webchannels.New(webchannels.Deps{
		Channels:   store,
		Access:     store,
		CSRFToken:  csrfTokenFromSessionContext,
		UserID:     userIDFromSessionContext,
		UserLabels: userLabels,
		Logger:     logger,
	})
	if err != nil {
		return nil, fmt.Errorf("channels_wire: build handler: %w", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux, nil
}

// Compile-time guard: the pgx adapter satisfies the channelsStore union.
var _ channelsStore = (*pgchannels.Store)(nil)
