package catalog_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/http/middleware/csp"
)

// TestList_CSPNonce_E2E pins SIN-63275 end-to-end for the catalog
// list page. csp.Middleware mints a fresh per-request nonce, the
// handler reads it via csp.Nonce(r.Context()) into pageData.CSPNonce,
// and the template emits it on the inline <style id="tenant-theme">.
// Byte-equality between the CSP header nonce and the rendered
// attribute is the invariant the browser enforces.
//
// Espelha TestLoginGet_CSPNonce_E2E em
// internal/adapter/httpapi/handler/login_csp_nonce_test.go (canonical
// reference PR #244 / SIN-63276).
func TestList_CSPNonce_E2E(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	seedProduct(t, store, "Mensalidade")
	mux := newHandler(t, store, resolverFromStore(t, store))

	srv := csp.Middleware(mux)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newRequest(t, http.MethodGet, "/catalog", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	header := rec.Header().Get(csp.HeaderName)
	if header == "" {
		t.Fatalf("missing %s header", csp.HeaderName)
	}
	nonce := extractStyleSrcNonceFromHeader(t, header)
	if nonce == "" {
		t.Fatalf("could not extract style-src nonce from header: %q", header)
	}

	body := rec.Body.String()
	wantTenantStyle := `<style id="tenant-theme" nonce="` + nonce + `">`
	if !strings.Contains(body, wantTenantStyle) {
		t.Fatalf("tenant-theme <style> missing matching nonce.\nheader nonce=%q\nwant fragment=%q\nbody=%q", nonce, wantTenantStyle, body)
	}

	// Defense against regressions: every inline <style>/<script> in
	// the body — i.e. every opening tag without a `src=` attribute —
	// must carry the header nonce.
	assertEveryInlineTagCarriesNonce(t, body, nonce)
}

// TestList_CSPNonce_FreshPerRequest pins the per-request freshness
// invariant: two GETs to the same handler mint different nonces and
// the body always tracks the request's own nonce. Without this
// guard, a leaked nonce could be replayed across requests.
func TestList_CSPNonce_FreshPerRequest(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))

	srv := csp.Middleware(mux)
	nonces := make([]string, 2)
	for i := range nonces {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, newRequest(t, http.MethodGet, "/catalog", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status=%d, want 200", i, rec.Code)
		}
		nonces[i] = extractStyleSrcNonceFromHeader(t, rec.Header().Get(csp.HeaderName))
		want := `nonce="` + nonces[i] + `"`
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("request %d: body did not carry header nonce %q", i, nonces[i])
		}
	}
	if nonces[0] == "" || nonces[0] == nonces[1] {
		t.Fatalf("expected fresh nonce per request, got %q twice", nonces[0])
	}
}

var styleSrcNoncePattern = regexp.MustCompile(`style-src [^;]*'nonce-([A-Za-z0-9_-]+)'`)

func extractStyleSrcNonceFromHeader(t *testing.T, header string) string {
	t.Helper()
	m := styleSrcNoncePattern.FindStringSubmatch(header)
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

// inlineTagOpenPattern matches `<style>` or `<script>` opening tags.
// External-source scripts (those carrying `src=`) skip the nonce
// requirement because CSP `script-src 'self'` already allows them.
var inlineTagOpenPattern = regexp.MustCompile(`<(style|script)\b([^>]*)>`)
var srcAttrPattern = regexp.MustCompile(`\bsrc\s*=`)

func assertEveryInlineTagCarriesNonce(t *testing.T, body, nonce string) {
	t.Helper()
	wantAttr := `nonce="` + nonce + `"`
	for _, m := range inlineTagOpenPattern.FindAllStringSubmatchIndex(body, -1) {
		tag := body[m[0]:m[1]]
		attrs := body[m[4]:m[5]]
		if srcAttrPattern.MatchString(attrs) {
			continue
		}
		if !strings.Contains(tag, wantAttr) {
			t.Fatalf("inline tag missing matching nonce: %q (want attr %q)", tag, wantAttr)
		}
	}
}
