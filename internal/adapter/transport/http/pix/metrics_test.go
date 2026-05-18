package pix_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	httppix "github.com/pericles-luz/crm/internal/adapter/transport/http/pix"
)

func TestMetrics_RegistersAndCountsOutcomes(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := httppix.NewMetrics(reg)

	m.Inc(httppix.OutcomeApplied)
	m.Inc(httppix.OutcomeApplied)
	m.Inc(httppix.OutcomeStuckPendingSuspected)

	if got := testutil.ToFloat64(m.Outcomes.WithLabelValues(string(httppix.OutcomeApplied))); got != 2 {
		t.Fatalf("applied counter = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.Outcomes.WithLabelValues(string(httppix.OutcomeStuckPendingSuspected))); got != 1 {
		t.Fatalf("stuck_pending_suspected counter = %v, want 1", got)
	}
}

func TestMetrics_HookReportsThroughCounter(t *testing.T) {
	t.Parallel()
	m := httppix.NewMetrics(nil)
	hook := m.Hook()
	if hook == nil {
		t.Fatal("Hook() on non-nil metrics must return a non-nil func")
	}
	hook(httppix.OutcomeDedupHit)
	hook(httppix.OutcomeDedupHit)
	hook(httppix.OutcomeReconcilerErr)

	if got := testutil.ToFloat64(m.Outcomes.WithLabelValues(string(httppix.OutcomeDedupHit))); got != 2 {
		t.Fatalf("dedup_hit counter via Hook = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.Outcomes.WithLabelValues(string(httppix.OutcomeReconcilerErr))); got != 1 {
		t.Fatalf("reconciler_err counter via Hook = %v, want 1", got)
	}
}

func TestMetrics_NilReceiverIsSafe(t *testing.T) {
	t.Parallel()
	var m *httppix.Metrics
	m.Inc(httppix.OutcomeApplied) // must not panic
	if hook := m.Hook(); hook != nil {
		t.Fatal("Hook() on nil receiver must return nil, got non-nil")
	}
}

func TestMetrics_NilRegistererBuildsUnregisteredVector(t *testing.T) {
	t.Parallel()
	m := httppix.NewMetrics(nil)
	if m.Outcomes == nil {
		t.Fatal("Outcomes must be non-nil even when registerer is nil")
	}
	// Re-register on a fresh registry to assert the vector is well-formed
	// independent of NewMetrics' registration step.
	reg := prometheus.NewRegistry()
	reg.MustRegister(m.Outcomes)
}
