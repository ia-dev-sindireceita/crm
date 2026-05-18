package pix

import "github.com/prometheus/client_golang/prometheus"

// Metrics is the Prometheus surface the Inter webhook receiver exports.
// One instance is constructed at boot via the wire and passed to the
// handler through InterWebhookConfig.MetricsHook (see Hook).
//
// Surface (SIN-63001):
//
//   - pix_inter_webhook_outcomes_total{outcome} — counter, one increment
//     per terminal classification of a /webhooks/pix/inter request.
//     Cardinality is bounded by the Outcome enum (applied, dedup_hit,
//     mixed, signature_fail, ip_fail, rate_limit_fail, parse_fail,
//     reconciler_err, body_read_fail, method_not_allowed,
//     stuck_pending_suspected). The stuck_pending_suspected label is the
//     dashboard alert path for SIN-62997 — WARN log alone is fragile
//     under log-volume spikes.
type Metrics struct {
	Outcomes *prometheus.CounterVec
}

// NewMetrics constructs the counter and registers it on reg. reg may be
// nil — then the vector is returned unregistered, which is the pattern
// tests use when isolation matters more than wiring into the global
// registry.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Outcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pix_inter_webhook_outcomes_total",
			Help: "Banco Inter PIX webhook receiver outcomes, partitioned by terminal classification (applied, dedup_hit, mixed, signature_fail, ip_fail, rate_limit_fail, parse_fail, reconciler_err, body_read_fail, method_not_allowed, stuck_pending_suspected).",
		}, []string{"outcome"}),
	}
	if reg != nil {
		reg.MustRegister(m.Outcomes)
	}
	return m
}

// Inc increments the outcomes counter for the given label. Safe to call
// on a nil receiver.
func (m *Metrics) Inc(outcome Outcome) {
	if m == nil {
		return
	}
	m.Outcomes.WithLabelValues(string(outcome)).Inc()
}

// Hook returns the MetricsHook function the InterWebhookConfig accepts.
// Returns nil on a nil receiver so callers can wire it unconditionally.
func (m *Metrics) Hook() func(Outcome) {
	if m == nil {
		return nil
	}
	return m.Inc
}
