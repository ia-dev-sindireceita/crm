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
	"log"
	"log/slog"
	"net/http"

	metricsusecase "github.com/pericles-luz/crm/internal/metrics/usecase"
	webdashboard "github.com/pericles-luz/crm/internal/web/dashboard"
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
func buildDashboardHandler(uc *metricsusecase.GetDashboard) http.Handler {
	if uc == nil {
		log.Printf("crm: dashboard UI disabled (metrics read-model not wired)")
		return nil
	}
	h, err := webdashboard.New(webdashboard.Deps{
		Snapshot:  uc,
		Logger:    slog.Default(),
		CSRFToken: csrfTokenFromSessionContext,
		UserID:    userIDFromSessionContext,
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
