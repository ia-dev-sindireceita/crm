package main

// SIN-65008 — managerial dashboard HTMX surface wireup (frontend half of
// the Dashboard / relatórios epic SIN-64963).
//
// buildDashboardHandler turns the SIN-65007 metrics read-model use case
// (built by buildMetricsDashboard) into the /dashboard HTMX mux. It owns
// no resources of its own — the pgxpool lifecycle stays with
// buildMetricsDashboard's cleanup — so this wire is a thin adapter: it
// returns nil when the use case is absent (DATABASE_URL unset) so the
// route stays unmounted on health-only / smoke boots, mirroring the
// fail-soft pattern of the other web/* wires.

import (
	"context"
	"log"
	"log/slog"
	"net/http"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	metricsusecase "github.com/pericles-luz/crm/internal/metrics/usecase"
	webdashboard "github.com/pericles-luz/crm/internal/web/dashboard"
	"github.com/pericles-luz/crm/internal/web/userlabel"
)

// buildDashboardHandler returns the /dashboard HTMX mux. The returned
// http.Handler is the stdlib *http.ServeMux produced by
// webdashboard.Handler.Routes; cmd/server hands it to httpapi.NewRouter
// via Deps.WebDashboard so chi wraps it with TenantScope + Auth + CSRF +
// RequireAuth + RequireAction(iam.ActionTenantContactRead) before
// dispatch.
//
// A nil use case (the buildMetricsDashboard fail-soft signal) yields a
// nil handler so the route stays unmounted. The concrete-pointer nil
// check is deliberate: assigning a typed nil *GetDashboard into the
// SnapshotUseCase interface would make the interface non-nil and defeat
// the New() guard, so we branch on the pointer before wrapping it.
// userLabels is variadic so the existing 1-arg test call sites keep
// compiling; the production path (main.go) passes the UserDirectory
// adapter built by buildDashboardUserDirectory as the optional second
// argument (SIN-65578). Only the first value is honoured; nil leaves the
// top bar on the "Conta" fallback.
func buildDashboardHandler(uc *metricsusecase.GetDashboard, userLabels ...userlabel.Directory) http.Handler {
	if uc == nil {
		log.Printf("crm: dashboard UI disabled (metrics read-model not wired)")
		return nil
	}
	var userDir userlabel.Directory
	if len(userLabels) > 0 {
		userDir = userLabels[0]
	}
	h, err := webdashboard.New(webdashboard.Deps{
		Snapshot:   uc,
		Logger:     slog.Default(),
		CSRFToken:  csrfTokenFromSessionContext,
		UserID:     userIDFromSessionContext,
		UserLabels: userDir,
	})
	if err != nil {
		// New only errors on a nil Snapshot, which the guard above rules
		// out; treat any error as "disable the surface" rather than
		// crashing the listener.
		log.Printf("crm: dashboard UI disabled — handler: %v", err)
		return nil
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	log.Printf("crm: dashboard UI wired on /dashboard")
	return mux
}

// buildDashboardUserDirectory builds the top-bar account-label resolver
// for the dashboard surface (SIN-65578). The dashboard owns no pool of
// its own (its metrics read-model lives behind buildMetricsDashboard's
// pool), so this opens a dedicated, short-lived pool just for the
// single-id label lookups and returns a cleanup that releases it.
//
// Fail-soft, matching every other web/* wire: a missing DSN or a connect
// fault yields (nil, no-op) so the caller passes a nil directory and the
// top bar degrades to the "Conta" fallback rather than downing the
// surface.
func buildDashboardUserDirectory(ctx context.Context, getenv func(string) string) (userlabel.Directory, func()) {
	noop := func() {}
	if getenv(pgpool.EnvDSN) == "" {
		return nil, noop
	}
	pool, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: dashboard top-bar label disabled — pg connect: %v", err)
		return nil, noop
	}
	dir, err := pginbox.NewUserDirectory(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: dashboard top-bar label disabled — user directory: %v", err)
		return nil, noop
	}
	return dir, func() { pool.Close() }
}
