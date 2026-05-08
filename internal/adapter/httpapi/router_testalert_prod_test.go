//go:build !test

package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestRouter_TestAlert_ProductionBuild_404 verifies that production
// builds (no `test` tag) serve 404 on /internal/test-alert and never
// touch rls_misses_total. The opposite assertion lives in
// router_testalert_tagged_test.go and runs under `-tags test`.
func TestRouter_TestAlert_ProductionBuild_404(t *testing.T) {
	t.Parallel()
	h, m, _, _, _ := newRouterWithObs(t)
	before := testutil.ToFloat64(m.RLSMisses)
	rec := do(t, h, http.MethodPost, "acme.crm.local", "/internal/test-alert", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("/internal/test-alert in prod build: got %d, want 404", rec.Code)
	}
	if testutil.ToFloat64(m.RLSMisses) != before {
		t.Errorf("rls_misses_total bumped in prod build")
	}
}
