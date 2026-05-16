package openrouter

import "github.com/prometheus/client_golang/prometheus"

// Metrics is the Prometheus surface for the OpenRouter adapter. One
// instance is constructed at boot and shared across goroutines —
// CounterVec / HistogramVec are concurrency-safe. Per SIN-62196 AC9
// dashboards consume:
//
//   - openrouter_request_duration_seconds{model,outcome}
//     where outcome ∈ {"ok","rate_limited","upstream_5xx","timeout",
//     "bad_request","invalid_response"}; latency is recorded on every
//     terminal attempt regardless of outcome so SLO panels can split
//     success vs failure.
//   - openrouter_tokens_consumed_total{model,direction}
//     where direction ∈ {"in","out"}; incremented only on successful
//     200 OK responses since failed calls did not actually consume
//     tokens upstream.
//
// Cardinality is bounded by the configured catalog (a handful of
// model strings); no tenant/user labels because those would explode
// the time-series count.
type Metrics struct {
	Registry        *prometheus.Registry
	RequestDuration *prometheus.HistogramVec
	TokensConsumed  *prometheus.CounterVec
}

// NewMetrics registers and returns the OpenRouter Prometheus surface.
// Tests that need isolation construct their own; cmd/server should call
// this once at boot and pass the registry to the obs HTTP handler.
//
// Histogram buckets follow the OpenRouter SLO (8s p99): a tail at 8s
// for the timeout signal, with a finer grain in the sub-second region
// where Gemini Flash actually lives.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "openrouter_request_duration_seconds",
			Help:    "OpenRouter chat-completions request duration in seconds, labelled by model and terminal outcome.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 4, 8, 16},
		}, []string{"model", "outcome"}),
		TokensConsumed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "openrouter_tokens_consumed_total",
			Help: "OpenRouter tokens consumed, labelled by model and direction (in=prompt, out=completion).",
		}, []string{"model", "direction"}),
	}
	reg.MustRegister(m.RequestDuration, m.TokensConsumed)
	return m
}

// observe is the nil-safe shortcut Client uses on every terminal
// outcome. The receiver is nil-safe so callers that did not wire
// Metrics (notably unit tests that don't care about the registry)
// compile and behave as no-ops.
func (m *Metrics) observe(model, outcome string, durationSeconds float64) {
	if m == nil {
		return
	}
	m.RequestDuration.WithLabelValues(model, outcome).Observe(durationSeconds)
}

// addTokens increments the in/out token counters on a successful call.
// Safe with a nil receiver for the same reason observe is.
func (m *Metrics) addTokens(model string, in, out int64) {
	if m == nil {
		return
	}
	if in > 0 {
		m.TokensConsumed.WithLabelValues(model, "in").Add(float64(in))
	}
	if out > 0 {
		m.TokensConsumed.WithLabelValues(model, "out").Add(float64(out))
	}
}
