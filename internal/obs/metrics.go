package obs

import (
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// defaultMetrics is the package-level Metrics pointer that adapter
// code (notably postgres.WithTenant) reaches for to record canary
// events without holding a *Metrics reference. atomic.Pointer makes
// SetDefault/IncRLSMiss safe across goroutines and across tests.
var defaultMetrics atomic.Pointer[Metrics]

// SetDefault wires m as the package-level metrics instance. cmd/server
// calls this once at boot. Tests use it (and reset to nil via
// SetDefault(nil) in t.Cleanup) to assert against rls_misses_total.
func SetDefault(m *Metrics) { defaultMetrics.Store(m) }

// Default returns the metrics instance set by SetDefault, or nil. The
// HTTP /metrics handler should be derived from the same instance.
func Default() *Metrics { return defaultMetrics.Load() }

// IncRLSMiss increments the package-level rls_misses_total counter.
// No-op when SetDefault has not yet been called, so adapter packages
// can call this unconditionally without crashing tests that did not
// wire metrics.
func IncRLSMiss() {
	if m := defaultMetrics.Load(); m != nil {
		m.IncRLSMiss()
	}
}

// Metrics is the small Prometheus surface SIN-62218 exposes. One
// instance is constructed at boot and shared via package-level
// helpers; tests that need isolation construct their own with
// NewMetrics.
type Metrics struct {
	Registry     *prometheus.Registry
	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec
	RLSMisses    prometheus.Counter
	// AuthRateLimitDenies counts 429s emitted by the
	// internal/adapter/httpapi/ratelimit middleware (SIN-62376).
	// Labels are policy ("login") + bucket ("ip" / "email") so the
	// dashboard can split per-bucket. Per-key cardinality is
	// deliberately excluded — the offending IP / email lives in the
	// log line, not the metric.
	AuthRateLimitDenies *prometheus.CounterVec
}

// NewMetrics builds a fresh registry plus the three SIN-62218
// instruments. Buckets follow the Prometheus default web-handler
// distribution (5ms..10s) so existing dashboard imports work.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests handled, partitioned by tenant, route, method, and status.",
		}, []string{"tenant", "route", "method", "status"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration in seconds, partitioned by tenant and route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"tenant", "route"}),
		RLSMisses: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "rls_misses_total",
			Help: "Times WithTenant was called with uuid.Nil — should always be 0; alarms wake oncall.",
		}),
		AuthRateLimitDenies: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "auth_ratelimit_deny_total",
			Help: "HTTP rate-limit 429s emitted by the auth middleware, partitioned by policy and bucket.",
		}, []string{"policy", "bucket"}),
	}
	reg.MustRegister(m.HTTPRequests, m.HTTPDuration, m.RLSMisses, m.AuthRateLimitDenies)
	return m
}

// AuthRateLimitDeny is the canonical OnDeny callback for the
// httpapi/ratelimit middleware. It increments the per-policy /
// per-bucket counter; key + retryAfter are intentionally dropped so
// the metric stays low-cardinality (the log line carries them). Safe
// with a nil receiver so wireup that omits Metrics still compiles.
func (m *Metrics) AuthRateLimitDeny(policy, bucket, _key string, _retryAfter time.Duration) {
	if m == nil {
		return
	}
	m.AuthRateLimitDenies.WithLabelValues(policy, bucket).Inc()
}

// Handler returns the http.Handler that exposes m's registry over
// /metrics. Callers wire this into a route that bypasses tenant /
// auth middleware; access control belongs at the network edge.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{
		Registry:          m.Registry,
		EnableOpenMetrics: false,
	})
}

// IncRLSMiss increments the canary counter. Safe to call with a nil
// receiver so adapters that lack a metrics hook (notably tests that
// don't wire obs) compile and behave as no-ops.
func (m *Metrics) IncRLSMiss() {
	if m == nil {
		return
	}
	m.RLSMisses.Inc()
}

// HTTPMetrics returns a chi/net-http compatible middleware that
// records request count and duration. Tenant label resolution is
// pluggable: the caller passes a tenantOf func so this package stays
// unaware of the tenancy types.
//
// Route is taken from chi's RouteContext if present; otherwise the
// raw URL path is used. Status is captured via a wrap-response writer.
func (m *Metrics) HTTPMetrics(tenantOf func(*http.Request) string, routeOf func(*http.Request) string) func(http.Handler) http.Handler {
	if m == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	if tenantOf == nil {
		tenantOf = func(*http.Request) string { return "" }
	}
	if routeOf == nil {
		routeOf = func(r *http.Request) string { return r.URL.Path }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(ww, r)
			tenant := tenantOf(r)
			route := routeOf(r)
			m.HTTPRequests.WithLabelValues(tenant, route, r.Method, strconv.Itoa(ww.status)).Inc()
			m.HTTPDuration.WithLabelValues(tenant, route).Observe(time.Since(start).Seconds())
		})
	}
}

// statusRecorder is a tiny http.ResponseWriter wrapper that captures
// the status code so HTTPMetrics can label requests by response
// outcome. WriteHeader-only — no body buffering.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}
