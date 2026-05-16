package wallet

import "github.com/prometheus/client_golang/prometheus"

// Outcome labels for wallet_allocation_total. The metric counts one
// per delivery handled by the Allocator. An operator alerting on a
// non-zero {outcome="failed_allocate"} rate sees idempotency or DB
// failure regardless of whether the message ultimately landed in the
// DLQ.
const (
	OutcomeAllocated        = "allocated"
	OutcomeSkippedDuplicate = "skipped_duplicate"
	OutcomeFailedDecode     = "failed_decode"
	OutcomeFailedPlan       = "failed_plan"
	OutcomeFailedAllocate   = "failed_allocate"
	OutcomeMissingMsgID     = "missing_msg_id"
)

// Metrics is the Prometheus surface the Allocator exports. One
// instance is constructed at boot and passed to New(); tests use a
// private Registry and assert via prometheus/testutil.
type Metrics struct {
	// Allocations counts allocator outcomes per delivery handled.
	// Labels: outcome.
	Allocations *prometheus.CounterVec

	// Lag observes the wall-clock delta between the event's
	// NewPeriodStart and the time the consumer handled it (in
	// seconds). CA #3 of SIN-62881 requires < 30s on the happy path;
	// operators alert on the p99 of this histogram.
	Lag prometheus.Histogram

	// HandleDuration observes the time spent inside the per-message
	// handler in seconds. Separate from Lag so dashboards can
	// distinguish consumer-side latency from upstream lag.
	HandleDuration prometheus.Histogram
}

// NewMetrics registers the allocator's counters on reg. reg may be
// nil for tests that want to build the counters without registering.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Allocations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wallet_allocation_total",
			Help: "Per-delivery wallet allocator outcomes (allocated, skipped_duplicate, failed_decode, failed_plan, failed_allocate, missing_msg_id).",
		}, []string{"outcome"}),
		Lag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "wallet_allocation_lag_seconds",
			Help:    "Lag between subscription.renewed event NewPeriodStart and consumer handling, in seconds.",
			Buckets: []float64{0.1, 0.5, 1, 5, 10, 30, 60, 300, 1800, 3600},
		}),
		HandleDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "wallet_allocation_duration_seconds",
			Help:    "Time spent inside the wallet allocator per-message handler, in seconds.",
			Buckets: prometheus.DefBuckets,
		}),
	}
	if reg != nil {
		reg.MustRegister(m.Allocations, m.Lag, m.HandleDuration)
	}
	return m
}

func (m *Metrics) incAllocation(outcome string) {
	if m == nil {
		return
	}
	m.Allocations.WithLabelValues(outcome).Inc()
}

func (m *Metrics) observeLag(seconds float64) {
	if m == nil {
		return
	}
	m.Lag.Observe(seconds)
}

func (m *Metrics) observeDuration(seconds float64) {
	if m == nil {
		return
	}
	m.HandleDuration.Observe(seconds)
}
