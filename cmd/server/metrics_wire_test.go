package main

// SIN-65007 — metrics dashboard wire tests. The aggregation adapter and
// use case carry their own coverage; this test only asserts the
// composition-root contract: buildMetricsDashboard returns (nil, no-op)
// when DATABASE_URL is unset so health-only / smoke boots stay clean, and
// the returned cleanup is always safe to call.

import (
	"context"
	"testing"
)

func TestBuildMetricsDashboard_DisabledWhenDSNUnset(t *testing.T) {
	uc, cleanup := buildMetricsDashboard(context.Background(), func(string) string { return "" })
	if uc != nil {
		t.Errorf("buildMetricsDashboard use case = %v, want nil when DSN unset", uc)
	}
	if cleanup == nil {
		t.Fatal("buildMetricsDashboard cleanup = nil, want non-nil no-op")
	}
	// The no-op cleanup must be safe to call unconditionally.
	cleanup()
}
