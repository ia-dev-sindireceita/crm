//go:build test

package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestRouter_TestAlert_TaggedBuild_BumpsCounter verifies the
// `-tags test` smoke-alert seam: POST /internal/test-alert returns
// 204 No Content and increments rls_misses_total once. Pairs with
// router_testalert_prod_test.go which holds the (default) 404
// behaviour.
func TestRouter_TestAlert_TaggedBuild_BumpsCounter(t *testing.T) {
	t.Parallel()
	h, m, _, _, _ := newRouterWithObs(t)
	before := testutil.ToFloat64(m.RLSMisses)
	rec := do(t, h, http.MethodPost, "acme.crm.local", "/internal/test-alert", nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("/internal/test-alert in tagged build: got %d, want 204", rec.Code)
	}
	if got := testutil.ToFloat64(m.RLSMisses) - before; got != 1 {
		t.Errorf("rls_misses_total: got %v increment, want 1", got)
	}
}
