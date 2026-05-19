package customdomain_verifier

import "github.com/prometheus/client_golang/prometheus"

// Metrics is the Prometheus surface the customdomain-verifier worker
// exports per SIN-63080. One instance is constructed at boot and passed
// to New; tests build a private registry and assert via
// prometheus/testutil.
//
//   - customdomain_verifier_cycles_total
//     counter — one increment per Tick start, regardless of outcome.
//   - customdomain_verifications_total{outcome}
//     counter — one increment per per-domain Verify attempt, labelled
//     by Outcome (verified / mismatch / resolver_error / blocked_ssrf
//     / already_verified / internal).
//   - customdomain_verifier_cycle_duration_seconds
//     histogram — wall-clock latency of one Tick.
//   - customdomain_verifier_pending_domains
//     gauge — number of rows the store returned this tick (visibility
//     into backlog depth).
//   - customdomain_verifier_giveup_total
//     counter — one increment per row that hit the attempt cap and
//     was marked failed.
type Metrics struct {
	CyclesTotal        prometheus.Counter
	VerificationsTotal *prometheus.CounterVec
	CycleDuration      prometheus.Histogram
	PendingDomains     prometheus.Gauge
	GiveUpTotal        prometheus.Counter
}

// NewMetrics registers the verifier counters on reg. reg may be nil for
// tests that want to construct the metrics without registering.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		CyclesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "customdomain_verifier_cycles_total",
			Help: "Number of Tick sweeps the customdomain verifier has started.",
		}),
		VerificationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "customdomain_verifications_total",
			Help: "Per-domain Verify attempts the worker dispatched, labelled by outcome.",
		}, []string{"outcome"}),
		CycleDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "customdomain_verifier_cycle_duration_seconds",
			Help:    "Wall-clock duration of one customdomain verifier tick.",
			Buckets: prometheus.DefBuckets,
		}),
		PendingDomains: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "customdomain_verifier_pending_domains",
			Help: "Number of pending_dns rows the store returned to the worker on the last tick.",
		}),
		GiveUpTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "customdomain_verifier_giveup_total",
			Help: "Number of domains the worker marked failed after exhausting the attempt cap.",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.CyclesTotal, m.VerificationsTotal, m.CycleDuration, m.PendingDomains, m.GiveUpTotal)
	}
	return m
}

func (m *Metrics) incCycles() {
	if m == nil {
		return
	}
	m.CyclesTotal.Inc()
}

func (m *Metrics) incOutcome(o Outcome) {
	if m == nil {
		return
	}
	m.VerificationsTotal.WithLabelValues(o.String()).Inc()
}

func (m *Metrics) observeCycle(seconds float64) {
	if m == nil {
		return
	}
	m.CycleDuration.Observe(seconds)
}

func (m *Metrics) setPending(n int) {
	if m == nil {
		return
	}
	m.PendingDomains.Set(float64(n))
}

func (m *Metrics) incGiveUp() {
	if m == nil {
		return
	}
	m.GiveUpTotal.Inc()
}
