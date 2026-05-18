package engine

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/google/uuid"
)

// Metrics is the four-instrument bundle the engine increments around
// every [Engine.Handle] call. The struct is created via [NewMetrics],
// which registers the instruments on the given registry — callers
// that boot multiple workers in the same process MUST hand each one
// its own [prometheus.Registry] to avoid a duplicate-registration
// panic (the standard CRM pattern, see internal/obs/metrics.go).
//
// The labels are intentionally low-cardinality:
//
//   - tenant: tenant uuid as string. Per-tenant grain is required for
//     the operator console.
//   - channel: 'whatsapp', 'webchat', 'instagram' — capped because the
//     codebase treats channel as a closed enum even though the
//     column is text.
//   - rule_id: uuid. High cardinality but bounded by rule count per
//     tenant; the metric is meant for "which rule fires most"
//     dashboards rather than global aggregations.
//   - action_type: 'move_to_stage' for Fase 4; widens with future
//     action kinds.
//
// Histogram buckets target sub-second evaluations on the happy path
// (one resolver query + one applications check + one stage move) with
// headroom for a slow Postgres roundtrip.
type Metrics struct {
	Evaluated *prometheus.CounterVec
	Matched   *prometheus.CounterVec
	Applied   *prometheus.CounterVec
	Latency   prometheus.Histogram
}

// NewMetrics constructs the bundle and registers each instrument on
// reg. A nil reg yields a no-op bundle the engine can still increment
// without erroring — useful in unit tests where metrics are out of
// scope.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Evaluated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "funnel_messages_evaluated_total",
			Help: "Total inbound messages the funnel engine evaluated, partitioned by tenant and channel.",
		}, []string{"tenant", "channel"}),
		Matched: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "funnel_rules_matched_total",
			Help: "Total rule matches per rule_id (cascade-resolved).",
		}, []string{"rule_id"}),
		Applied: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "funnel_actions_applied_total",
			Help: "Total successful action dispatches per action_type.",
		}, []string{"action_type"}),
		Latency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "funnel_evaluation_latency_seconds",
			Help:    "Latency of a single Engine.Handle call, from decode to record (post-action).",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}),
	}
	if reg != nil {
		reg.MustRegister(m.Evaluated, m.Matched, m.Applied, m.Latency)
	}
	return m
}

// observeEvaluated is the safe accessor the engine calls on every
// inbound. Nil-receiver tolerated so tests that pass a nil Metrics
// don't NPE.
func (m *Metrics) observeEvaluated(tenant uuid.UUID, channel string) {
	if m == nil {
		return
	}
	m.Evaluated.WithLabelValues(tenant.String(), channel).Inc()
}

func (m *Metrics) observeMatched(ruleID uuid.UUID) {
	if m == nil {
		return
	}
	m.Matched.WithLabelValues(ruleID.String()).Inc()
}

func (m *Metrics) observeApplied(actionType string) {
	if m == nil {
		return
	}
	m.Applied.WithLabelValues(actionType).Inc()
}

func (m *Metrics) observeLatency(seconds float64) {
	if m == nil {
		return
	}
	m.Latency.Observe(seconds)
}
