package main

// SIN-63105 — wire test pinning the metric plumbing buildBrandingStack
// gained when cmd/server stopped passing nil for the second argument.
// Previously the SIN-63085 theme middleware was wired with a nil
// ThemeMetrics so tenant_theme_cache_hits_total stayed silent in
// production even though AC #3 of SIN-63085 calls for it to be live.
//
// The test drives one no-tenant lookup through the bundled
// ThemeMiddleware and asserts the counter incremented on the *exact*
// obs.Metrics instance the caller handed in — i.e. the same registry
// the SIN-62218 /metrics scrape endpoint exposes.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/obs"
)

func TestBuildBrandingStack_ThreadsMetricsIntoThemeMiddleware(t *testing.T) {
	t.Parallel()
	m := obs.NewMetrics()
	s := buildBrandingStack(quietLogger(), m)
	t.Cleanup(s.Cleanup)

	// Drive one request through the bundled Theme.Handler. The request
	// carries no tenant on the context, so the middleware records the
	// "no_tenant" branch — a deterministic, single-shot signal that
	// the metrics seam is wired without requiring a tenancy fixture.
	inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	req := httptest.NewRequest(http.MethodGet, "/branding", nil)
	rec := httptest.NewRecorder()
	s.Theme.Handler(inner).ServeHTTP(rec, req)

	got := testutil.ToFloat64(
		m.TenantThemeCacheHits.WithLabelValues(middleware.ThemeCacheResultNoTenant),
	)
	if got != 1 {
		t.Fatalf("tenant_theme_cache_hits_total{result=%q} = %v, want 1 — "+
			"buildBrandingStack must pass the metrics argument through to the "+
			"theme middleware so SIN-63085 AC #3 observability is live",
			middleware.ThemeCacheResultNoTenant, got)
	}
}
