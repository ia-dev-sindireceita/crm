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

// TestBaseTemplate_SRIAttributeOnEveryVendorScript is the snapshot
// regression for SIN-62535. Every `<script src="/static/vendor/...">`
// in the rendered base template must carry the `integrity="sha384-"`
// + `crossorigin="anonymous"` attribute pair produced by vendorSRI so
// the browser re-verifies the bytes it executes. The test scans the
// whole rendered body and asserts each vendored script tag — adding a
// new vendored bundle without wiring vendorSRI will fail here.
//
// We render the full `base` template via the real serveList path —
// that's the production wiring, including the embed-backed
// CHECKSUMS.txt.
func TestBaseTemplate_SRIAttributeOnEveryVendorScript(t *testing.T) {
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

	tags := scanVendorScriptTags(body)
	if len(tags) == 0 {
		t.Fatalf("base template renders no <script src=\"/static/vendor/...\"> tags; body=%s", body)
	}
	for _, tag := range tags {
		if !strings.Contains(tag, `integrity="sha384-`) {
			t.Errorf("vendored <script> missing integrity=sha384-: %s", tag)
		}
		if !strings.Contains(tag, `crossorigin="anonymous"`) {
			t.Errorf("vendored <script> missing crossorigin=anonymous: %s", tag)
		}
	}
}

// scanVendorScriptTags returns every full `<script ... src="/static/vendor/...">`
// opening tag in body. Helper for SIN-62535 snapshot tests — the regex-
// free scan keeps it html/template-safe and tolerates attribute order.
func scanVendorScriptTags(body string) []string {
	const needle = `src="/static/vendor/`
	var out []string
	rest := body
	for {
		srcIdx := strings.Index(rest, needle)
		if srcIdx < 0 {
			return out
		}
		// Walk backwards to the enclosing `<script` open token.
		openIdx := strings.LastIndex(rest[:srcIdx], "<script")
		if openIdx < 0 {
			// `src="/static/vendor/...` appeared outside a <script>;
			// skip past this hit so we keep scanning.
			rest = rest[srcIdx+len(needle):]
			continue
		}
		// Find the end of the open tag — first `>` after openIdx.
		closeIdx := strings.Index(rest[openIdx:], ">")
		if closeIdx < 0 {
			return out
		}
		end := openIdx + closeIdx + 1
		out = append(out, rest[openIdx:end])
		rest = rest[end:]
	}
}
