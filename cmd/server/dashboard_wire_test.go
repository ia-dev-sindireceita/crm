package main

// SIN-65008 — dashboard handler wire tests. The web handler + metrics
// read-model carry their own coverage; this test pins the composition-
// root contract: buildDashboardHandler returns nil when the metrics use
// case is absent (the buildMetricsDashboard fail-soft signal) so the
// /dashboard routes stay unmounted, and returns a non-nil mux when a real
// use case is supplied.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/metrics"
	metricsusecase "github.com/pericles-luz/crm/internal/metrics/usecase"
)

// fakeMetricsReader is a no-DB metrics.Reader so the wire test can build a
// real *GetDashboard without a pgxpool. Snapshot is never called here.
type fakeMetricsReader struct{}

func (fakeMetricsReader) Snapshot(context.Context, uuid.UUID, time.Time) (metrics.DashboardMetrics, error) {
	return metrics.DashboardMetrics{}, nil
}

func TestBuildDashboardHandler_NilUseCaseDisablesRoute(t *testing.T) {
	if h := buildDashboardHandler(nil); h != nil {
		t.Errorf("buildDashboardHandler(nil) = %v, want nil (route unmounted)", h)
	}
}

func TestBuildDashboardHandler_WiresMuxWhenUseCasePresent(t *testing.T) {
	uc, err := metricsusecase.NewGetDashboard(fakeMetricsReader{})
	if err != nil {
		t.Fatalf("NewGetDashboard: %v", err)
	}

	// Passing a nil directory (the SIN-65578 fail-soft signal) must still
	// wire the mux — the top bar degrades to the "Conta" fallback rather
	// than dropping the surface.
	h := buildDashboardHandler(uc, nil)
	if h == nil {
		t.Fatal("buildDashboardHandler(uc) = nil, want a routed mux")
	}

	// The mux must 404 an unknown path, proving Routes registered the
	// expected patterns without us having to drive a tenant-scoped
	// request (and therefore without touching Snapshot / the DB).
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/not-a-dashboard-route", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d for unknown path, want 404 from the wired mux", rec.Code)
	}
}

// TestBuildDashboardUserDirectory_DisabledWhenDSNUnset pins the SIN-65578
// composition-root contract: with no DATABASE_URL the top-bar label
// resolver is (nil, no-op) so health-only / smoke boots stay clean and the
// dashboard degrades to the "Conta" fallback. The no-op cleanup must be
// safe to call unconditionally.
func TestBuildDashboardUserDirectory_DisabledWhenDSNUnset(t *testing.T) {
	dir, cleanup := buildDashboardUserDirectory(context.Background(), func(string) string { return "" })
	if dir != nil {
		t.Errorf("buildDashboardUserDirectory directory = %v, want nil when DSN unset", dir)
	}
	if cleanup == nil {
		t.Fatal("buildDashboardUserDirectory cleanup = nil, want non-nil no-op")
	}
	cleanup()
}
