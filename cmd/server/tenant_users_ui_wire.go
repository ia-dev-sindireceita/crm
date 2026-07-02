package main

// SIN-66499 wiring — HTMX tenant user-management admin UI. Mounts the routes
// under /settings/users backed by the tenantusers Postgres adapter
// (tenantusers.Repository) + the ADR 0070 Argon2id hasher + the split-audit
// logger. Only the runtime pool (app_runtime, RLS-gated) is needed: the
// users_master_ops_audit trigger is a no-op for app_runtime, so the writes
// route through the same pool as the reads. When DATABASE_URL is unset or
// unreachable the wire returns a nil handler and the router leaves the
// /settings/users routes unmounted — the same fail-soft pattern as the other
// web/* wires.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	pgtenantusers "github.com/pericles-luz/crm/internal/adapter/db/postgres/tenantusers"
	"github.com/pericles-luz/crm/internal/iam/password"
	"github.com/pericles-luz/crm/internal/tenantusers"
	webtenantusers "github.com/pericles-luz/crm/internal/web/tenantusers"
	"github.com/pericles-luz/crm/internal/web/userlabel"
)

// buildWebTenantUsersHandler returns the user-management admin mux + a
// cleanup closure that releases the pgxpool the wire opened. A nil handler
// signals "skip mounting" so callers can defer the cleanup unconditionally.
func buildWebTenantUsersHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	noop := func() {}
	if getenv(pgpool.EnvDSN) == "" {
		log.Printf("crm: web/tenantusers disabled — DATABASE_URL unset")
		return nil, noop
	}
	pool, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: web/tenantusers disabled — pg runtime connect: %v", err)
		return nil, noop
	}
	store, err := pgtenantusers.New(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/tenantusers disabled — users store: %v", err)
		return nil, noop
	}
	// The user-label directory is best-effort chrome: a failure degrades the
	// app-shell account label to the shell fallback, it does not disable the
	// surface.
	var userDir userlabel.Directory
	if dir, derr := pginbox.NewUserDirectory(pool); derr != nil {
		log.Printf("crm: web/tenantusers — user directory unavailable, using shell fallback: %v", derr)
	} else {
		userDir = dir
	}
	// User create / role-change / deactivate are privilege events that must
	// land in audit_log_security (SIN-66499). Route them through the shared
	// SplitLogger on the RLS-gated runtime pool. A logger-build failure
	// degrades to no audit emission rather than disabling the surface.
	var auditor tenantusers.Auditor
	if splitLogger, aerr := pgpool.NewSplitAuditLogger(pool); aerr != nil {
		log.Printf("crm: web/tenantusers — audit disabled: %v", aerr)
	} else {
		auditor = newTenantUserAuditor(splitLogger, slog.Default())
	}
	handler, err := assembleWebTenantUsersHandler(store, password.Default(), auditor, userDir, slog.Default())
	if err != nil {
		pool.Close()
		log.Printf("crm: web/tenantusers disabled — assemble: %v", err)
		return nil, noop
	}
	log.Printf("crm: web/tenantusers HTMX routes mounted (tenantusers adapter wired)")
	return handler, func() { pool.Close() }
}

// assembleWebTenantUsersHandler is the pure assembly seam. Tests call it
// directly with a stub repo/hasher so the wire is exercised without booting
// the whole server.
func assembleWebTenantUsersHandler(
	repo tenantusers.Repository,
	hasher tenantusers.PasswordHasher,
	auditor tenantusers.Auditor,
	userLabels userlabel.Directory,
	logger *slog.Logger,
) (http.Handler, error) {
	if repo == nil {
		return nil, errors.New("tenant_users_wire: repo is nil")
	}
	if hasher == nil {
		return nil, errors.New("tenant_users_wire: hasher is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	svc, err := tenantusers.NewService(repo, hasher, auditor)
	if err != nil {
		return nil, fmt.Errorf("tenant_users_wire: build service: %w", err)
	}
	h, err := webtenantusers.New(webtenantusers.Deps{
		Users:      svc,
		CSRFToken:  csrfTokenFromSessionContext,
		UserID:     userIDFromSessionContext,
		UserLabels: userLabels,
		Logger:     logger,
	})
	if err != nil {
		return nil, fmt.Errorf("tenant_users_wire: build handler: %w", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux, nil
}

// Compile-time guard: the pgx adapter satisfies the Repository port.
var _ tenantusers.Repository = (*pgtenantusers.Store)(nil)
