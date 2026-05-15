package whatsapp

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// handlerMetrics is the inbound-handler latency surface. SIN-62762
// monitors the synchronous message loop in handlePost against Meta's
// documented 5s response budget — see
// docs/runbooks/whatsapp-inbound-latency.md for the operational
// threshold + escalation path.
//
// Bucket layout below intentionally pins boundaries at 3s (the
// runbook's escalation trigger) and 5s (Meta's documented limit) so
// p99 alerts have sharp signals to fire on without re-bucketing.
type handlerMetrics struct {
	duration *prometheus.HistogramVec
}

// newHandlerMetrics registers whatsapp_handler_elapsed_seconds on reg.
// reg MUST be non-nil; production wires prometheus.DefaultRegisterer
// and tests inject a fresh prometheus.NewRegistry() to avoid
// duplicate-registration panics.
func newHandlerMetrics(reg prometheus.Registerer) *handlerMetrics {
	m := &handlerMetrics{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "whatsapp_handler_elapsed_seconds",
			Help: "End-to-end POST /webhooks/whatsapp handler latency in seconds, partitioned by terminal result.",
			Buckets: []float64{
				0.005, 0.025, 0.1, 0.25, 0.5, 1, 2, 3, 5, 10,
			},
		}, []string{"result"}),
	}
	reg.MustRegister(m.duration)
	return m
}

// observe records one handler exit. nil receiver is a no-op so cmd/server
// can omit WithMetricsRegistry in restricted boots (e.g. a build that
// has not yet wired Prometheus) without crashing.
func (m *handlerMetrics) observe(result string, elapsed time.Duration) {
	if m == nil {
		return
	}
	secs := elapsed.Seconds()
	if secs < 0 {
		// Clock skew between Now() calls inside a single request is not
		// expected (monotonic clock under stdlib time), but a fakeClock
		// in tests could advance backwards. Clamp to 0 so the histogram
		// keeps a bounded domain.
		secs = 0
	}
	m.duration.WithLabelValues(result).Observe(secs)
}
