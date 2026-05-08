//go:build test

package obs_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pericles-luz/crm/internal/obs"
)

// Under `-tags test` the smoke-alert handler increments
// rls_misses_total once and returns 204. This is the path
// `make smoke-alert` exercises in stg.
func TestTestAlertHandler_Tagged_BumpsCounter(t *testing.T) {
	t.Parallel()
	m := obs.NewMetrics()
	h := obs.TestAlertHandler(m)

	req := httptest.NewRequest(http.MethodPost, "/internal/test-alert", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status: got %d, want 204", rec.Code)
	}
	if got := testutil.ToFloat64(m.RLSMisses); got != 1 {
		t.Errorf("rls_misses_total: got %v, want 1", got)
	}
}
