package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
)

// newMasterCSRFRejectionCounter builds the OnReject hook the router wires
// onto both master Origin/Referer CSRF gates (the /m/* bootstrap group and
// the relocated /master/* operator surface). Each rejection increments
// master_csrf_rejected_total{reason}, where reason is one of the stable
// OriginCSRFReason label values declared in the mastermfa package
// (host_unset / missing / unparsable / scheme_not_https / mismatch).
//
// Cardinality is bounded by that fixed enum, so the series count is
// constant regardless of attacker volume. The counter backs a CSRF-probe
// alert against the master operator surface (which fronts impersonate),
// turning a previously log-only signal (the master_csrf_rejected slog
// event) into a metric (SIN-65276).
//
// reg may be nil — then the counter is built unregistered, the same
// isolation seam authz.NewMetrics uses so tests avoid the global registry
// and a duplicate-registration panic. The returned hook is always safe to
// call.
func newMasterCSRFRejectionCounter(reg prometheus.Registerer) func(*http.Request, mastermfa.OriginCSRFReason) {
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "master_csrf_rejected_total",
		Help: "Master console Origin/Referer CSRF rejections by cause. Backs a CSRF-probe alert on the master operator surface; bounded cardinality (one series per OriginCSRFReason).",
	}, []string{"reason"})
	if reg != nil {
		reg.MustRegister(counter)
	}
	return func(_ *http.Request, reason mastermfa.OriginCSRFReason) {
		counter.WithLabelValues(string(reason)).Inc()
	}
}
