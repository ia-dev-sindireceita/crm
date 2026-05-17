package aiassistinvalidator

import "github.com/prometheus/client_golang/prometheus"

// Outcome labels for the invalidation_total counter. The closed set
// keeps cardinality bounded.
const (
	OutcomeInvalidated      = "invalidated"
	OutcomeFailedDecode     = "failed_decode"
	OutcomeMissingIDs       = "missing_ids"
	OutcomeFailedInvalidate = "failed_invalidate"
)

// Metrics is the Prometheus surface the worker emits. nil values
// allowed throughout so tests do not have to register with a global
// registry.
type Metrics struct {
	Duration         prometheus.Histogram
	InvalidatedTotal *prometheus.CounterVec
}

// NewMetrics constructs the metric surface against reg. reg may be
// nil — the metrics are returned unregistered, the pattern tests use
// to keep registries isolated.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "aiassist_invalidator_handle_duration_seconds",
			Help:    "End-to-end aiassist_invalidator handle latency in seconds. [SIN-62908]",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2},
		}),
		InvalidatedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aiassist_invalidator_outcomes_total",
			Help: "AISummary invalidation outcomes, labelled by outcome (invalidated | failed_decode | missing_ids | failed_invalidate). [SIN-62908]",
		}, []string{"outcome"}),
	}
	if reg != nil {
		reg.MustRegister(m.Duration, m.InvalidatedTotal)
	}
	return m
}

// observeDuration is the nil-tolerant emit path.
func (m *Metrics) observeDuration(seconds float64) {
	if m == nil || m.Duration == nil {
		return
	}
	m.Duration.Observe(seconds)
}

// incOutcome is the nil-tolerant emit path.
func (m *Metrics) incOutcome(outcome string) {
	if m == nil || m.InvalidatedTotal == nil {
		return
	}
	m.InvalidatedTotal.WithLabelValues(outcome).Inc()
}
