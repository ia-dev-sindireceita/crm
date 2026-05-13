package customdomain_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/customdomain/management"
)

// TestBaseTemplate_SRIAttributeOnHTMX is the snapshot assertion for
// SIN-62535: every vendored <script> tag in base.html must carry the
// `integrity="sha384-"` attribute pair produced by vendorSRI so the
// browser re-verifies the bytes it executes.
//
// We render the full `base` template via the real serveList path —
// that's the production wiring, including the embed-backed
// CHECKSUMS.txt — and assert the integrity substring is present.
func TestBaseTemplate_SRIAttributeOnHTMX(t *testing.T) {
	t.Parallel()
	verified := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	uc := &fakeUseCase{
		listResp: []management.Domain{
			{
				ID:                 uuid.New(),
				TenantID:           testTenant,
				Host:               "shop.example.com",
				VerifiedAt:         &verified,
				VerifiedWithDNSSEC: true,
				CreatedAt:          verified,
				UpdatedAt:          verified,
			},
		},
	}
	h := newHandlerForTest(t, uc)
	mux := newServeMux(h)
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest(http.MethodGet, "/tenant/custom-domains", nil), testTenant)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	body := rec.Body.String()

	// Every vendored <script> in base.html must carry both halves of
	// the SRI attribute pair. The crossorigin half is required by the
	// SRI spec for cross-origin <script> tags; we emit it
	// unconditionally so future CDN deployments are covered.
	const wantIntegrityPrefix = `integrity="sha384-`
	const wantCrossorigin = `crossorigin="anonymous"`
	if !strings.Contains(body, wantIntegrityPrefix) {
		t.Fatalf("base template missing %q; body=%s", wantIntegrityPrefix, body)
	}
	if !strings.Contains(body, wantCrossorigin) {
		t.Fatalf("base template missing %q; body=%s", wantCrossorigin, body)
	}

	// Defence-in-depth: confirm the integrity attribute is anchored to
	// the htmx <script> tag, not some unrelated element that happened
	// to land in the page. Scan from the htmx src= and check that an
	// integrity= attribute appears before the next `</script>` close.
	htmxIdx := strings.Index(body, `src="/static/vendor/htmx/2.0.9/htmx.min.js"`)
	if htmxIdx < 0 {
		t.Fatalf("htmx <script> tag missing from rendered base")
	}
	closeIdx := strings.Index(body[htmxIdx:], "</script>")
	if closeIdx < 0 {
		t.Fatalf("htmx <script> tag is unclosed in rendered base")
	}
	scriptSegment := body[htmxIdx : htmxIdx+closeIdx]
	if !strings.Contains(scriptSegment, wantIntegrityPrefix) {
		t.Fatalf("htmx <script> segment missing integrity attribute: %s", scriptSegment)
	}
	if !strings.Contains(scriptSegment, wantCrossorigin) {
		t.Fatalf("htmx <script> segment missing crossorigin attribute: %s", scriptSegment)
	}
}

