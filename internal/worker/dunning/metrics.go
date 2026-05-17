package dunning

import "github.com/prometheus/client_golang/prometheus"

// Metrics is the Prometheus surface the dunning tick worker exports.
// One instance is constructed at boot and passed to New(); tests build
// a private registry and assert via prometheus/testutil.
//
// The shape matches the SIN-62965 spec:
//
//   - dunning_state_total{state}   gauge — count of subscriptions currently
//     in each state (rebuilt each tick from the listing query).
//   - dunning_transitions_total{from,to}
//     counter — every applied transition.
//   - dunning_tick_latency_seconds  histogram — wall-clock time per Tick.
type Metrics struct {
	StateGauge       *prometheus.GaugeVec
	TransitionsTotal *prometheus.CounterVec
	TickLatency      prometheus.Histogram
}

// NewMetrics registers the dunning counters on reg. reg may be nil for
// tests that want to construct the metrics without registering.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		StateGauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dunning_state_total",
			Help: "Number of subscriptions currently in each dunning state (gauge, rebuilt every tick).",
		}, []string{"state"}),
		TransitionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dunning_transitions_total",
			Help: "Dunning transitions applied by the tick worker, partitioned by from/to state.",
		}, []string{"from", "to"}),
		TickLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "dunning_tick_latency_seconds",
			Help:    "Wall-clock latency of one dunning tick.",
			Buckets: prometheus.DefBuckets,
		}),
	}
	if reg != nil {
		reg.MustRegister(m.StateGauge, m.TransitionsTotal, m.TickLatency)
	}
	return m
}

func (m *Metrics) observeTick(seconds float64) {
	if m == nil {
		return
	}
	m.TickLatency.Observe(seconds)
}

func (m *Metrics) incTransition(from, to string) {
	if m == nil {
		return
	}
	m.TransitionsTotal.WithLabelValues(from, to).Inc()
}

func (m *Metrics) setStateCount(state string, n int) {
	if m == nil {
		return
	}
	m.StateGauge.WithLabelValues(state).Set(float64(n))
}
