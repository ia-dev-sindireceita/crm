//go:build test

package obs

import "net/http"

// TestAlertHandler returns a handler that increments rls_misses_total
// on each invocation. It is the stg-only smoke-test seam wired by
// `make smoke-alert`: a synthetic increment trips the
// RLSMissDetected alert in Prometheus, which Alertmanager forwards
// to Slack #alerts. The handler is mounted only when binaries are
// built with `-tags test`; production binaries get the no-op variant
// in testalert_prod.go (which serves 404).
func TestAlertHandler(m *Metrics) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		m.IncRLSMiss()
		w.WriteHeader(http.StatusNoContent)
	}
}
