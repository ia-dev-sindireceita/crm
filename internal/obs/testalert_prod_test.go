//go:build !test

package obs_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pericles-luz/crm/internal/obs"
)

// In production builds the TestAlertHandler is a 404 and never
// touches rls_misses_total — the smoke-test seam is opt-in via
// `-tags test`. Compiled only when the `test` build tag is NOT set.
func TestTestAlertHandler_Prod_Is404AndDoesNotIncrement(t *testing.T) {
	t.Parallel()
	m := obs.NewMetrics()
	h := obs.TestAlertHandler(m)

	req := httptest.NewRequest(http.MethodPost, "/internal/test-alert", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	if got := testutil.ToFloat64(m.RLSMisses); got != 0 {
		t.Errorf("rls_misses_total bumped in prod build: got %v", got)
	}
}
