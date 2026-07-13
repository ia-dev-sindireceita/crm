package main

// SIN-66496 (child of SIN-66493) wiring — HTMX tenant user-management admin
// UI. Mounts /settings/users backed by the tenantusers Postgres adapter, the
// argon2id hasher, and the shared SplitLogger for security audit.
//
// Only the runtime pool (app_runtime, RLS-gated) is needed: creates/role-
// changes/deactivations run inside the tenant's RLS envelope. When
// DATABASE_URL is unset or unreachable the wire returns a nil handler and the
// router leaves the /settings/users routes unmounted — the same fail-soft
// pattern as the other web/* wires.
//
// Unlike the best-effort channel-access audit, the security audit here is a
// hard dependency: user create / role-change / deactivate MUST be auditable
// (SIN-66494 §3), so a SplitLogger build failure disables the surface rather
// than silently dropping the trail.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgtenantusers "github.com/pericles-luz/crm/internal/adapter/db/postgres/tenantusers"
	"github.com/pericles-luz/crm/internal/iam/password"
	"github.com/pericles-luz/crm/internal/tenantusers"
	webusers "github.com/pericles-luz/crm/internal/web/users"
)

// buildWebUsersHandler returns the tenant user-management mux + a cleanup
// closure that releases the pgxpool the wire opened. A nil handler signals
// "skip mounting" so callers can defer the cleanup unconditionally.
func buildWebUsersHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	noop := func() {}
	if getenv(pgpool.EnvDSN) == "" {
		log.Printf("crm: web/users disabled — DATABASE_URL unset")
		return nil, noop
	}
	pool, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: web/users disabled — pg runtime connect: %v", err)
		return nil, noop
	}
	store, err := pgtenantusers.New(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/users disabled — tenantusers store: %v", err)
		return nil, noop
	}
	auditor, err := pgpool.NewSplitAuditLogger(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/users disabled — audit logger: %v", err)
		return nil, noop
	}
	handler, err := assembleWebUsersHandler(store, password.Default(), auditor, slog.Default())
	if err != nil {
		pool.Close()
		log.Printf("crm: web/users disabled — assemble: %v", err)
		return nil, noop
	}
	log.Printf("crm: web/users HTMX routes mounted (tenantusers adapter wired)")
	return handler, func() { pool.Close() }
}

// assembleWebUsersHandler is the pure assembly seam. Tests call it directly
// with a fake repository so the wire is exercised without booting the server.
func assembleWebUsersHandler(repo tenantusers.Repository, hasher tenantusers.Hasher, auditor tenantusers.Auditor, logger *slog.Logger) (http.Handler, error) {
	if repo == nil {
		return nil, errors.New("users_wire: repo is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	svc, err := tenantusers.NewService(tenantusers.Config{
		Repo:    repo,
		Hasher:  hasher,
		Auditor: auditor,
		Logger:  logger,
	})
	if err != nil {
		return nil, fmt.Errorf("users_wire: build service: %w", err)
	}
	h, err := webusers.New(webusers.Deps{
		Service:   svc,
		CSRFToken: csrfTokenFromSessionContext,
		Logger:    logger,
	})
	if err != nil {
		return nil, fmt.Errorf("users_wire: build handler: %w", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux, nil
}

// Compile-time guard: the pgx adapter satisfies the domain Repository port.
var _ tenantusers.Repository = (*pgtenantusers.Store)(nil)
