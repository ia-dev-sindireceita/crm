//go:build !test

package obs

import "net/http"

// TestAlertHandler returns a 404 handler in production builds. The
// real implementation lives in testalert_test_build.go and is only
// compiled when binaries are built with `-tags test`. Keeping the
// symbol present in both build modes lets the router wire the route
// unconditionally without a build-tagged switch at the call site.
func TestAlertHandler(_ *Metrics) http.HandlerFunc {
	return http.NotFound
}
