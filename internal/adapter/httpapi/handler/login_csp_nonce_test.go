package handler_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/handler"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
)

// TestLoginGet_CSPNonce_E2E pins the SIN-63275 fix end-to-end. The
// CSP middleware mints a fresh nonce per request and stamps it on the
// Content-Security-Policy header; the layout reads it back via
// csp.Nonce → cspNonce FuncMap helper and emits it on the
// <style id="tenant-theme"> tag. The browser only renders the inline
// stylesheet if the nonce attribute matches the nonce-source in the
// header — so this test asserts byte-equality between the two.
//
// The production bug this regression-tests: before SIN-63275, the
// layout emitted <style id="tenant-theme"> with no nonce attribute,
// which the strict policy (`style-src 'self' 'nonce-…'` — no
// `'unsafe-inline'`) blocked, breaking /login.
func TestLoginGet_CSPNonce_E2E(t *testing.T) {
	t.Parallel()

	srv := csp.Middleware(http.HandlerFunc(handler.LoginGet))

	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%q", rec.Code, rec.Body.String())
	}

	cspHeader := rec.Header().Get(csp.HeaderName)
	if cspHeader == "" {
		t.Fatalf("missing %s header", csp.HeaderName)
	}

	nonce := extractStyleSrcNonce(t, cspHeader)
	if nonce == "" {
		t.Fatalf("could not extract style-src nonce from header: %q", cspHeader)
	}

	body := rec.Body.String()

	// Every <style> the layout owns must carry exactly this nonce. A
	// substring match against the literal attribute is enough: empty
	// or wrong nonces would never produce this exact fragment.
	wantTenantStyle := `<style id="tenant-theme" nonce="` + nonce + `">`
	if !strings.Contains(body, wantTenantStyle) {
		t.Fatalf("tenant-theme <style> missing matching nonce.\nheader nonce=%q\nwant fragment=%q\nbody=%q", nonce, wantTenantStyle, body)
	}

	// Defense against regressions that emit additional <style> or
	// <script> tags without a nonce. Walk every inline tag in the
	// body and assert each carries `nonce="<header-nonce>"`. We do
	// NOT match externally-sourced tags here — layout has none — but
	// the regex below tolerates `src=…` should the layout grow them.
	assertEveryInlineTagCarriesNonce(t, body, nonce)
}

// TestLoginGet_CSPNonce_FreshPerRequest pins the per-request nonce
// invariant: two GETs to the same handler mint different nonces in
// the header, and the body always tracks the request's own nonce.
// Without per-request freshness, a leaked nonce could be replayed.
func TestLoginGet_CSPNonce_FreshPerRequest(t *testing.T) {
	t.Parallel()

	srv := csp.Middleware(http.HandlerFunc(handler.LoginGet))

	nonces := make([]string, 2)
	for i := range nonces {
		r := httptest.NewRequest(http.MethodGet, "/login", nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200", rec.Code)
		}
		nonces[i] = extractStyleSrcNonce(t, rec.Header().Get(csp.HeaderName))
		want := `nonce="` + nonces[i] + `">`
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("request %d: body did not carry header nonce %q", i, nonces[i])
		}
	}
	if nonces[0] == "" || nonces[0] == nonces[1] {
		t.Fatalf("expected fresh nonce per request, got %q twice", nonces[0])
	}
}

// styleSrcNoncePattern extracts the literal nonce value from a
// `style-src 'self' 'nonce-{value}'` clause in the CSP header.
var styleSrcNoncePattern = regexp.MustCompile(`style-src [^;]*'nonce-([A-Za-z0-9_-]+)'`)

func extractStyleSrcNonce(t *testing.T, header string) string {
	t.Helper()
	m := styleSrcNoncePattern.FindStringSubmatch(header)
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

// inlineTagPattern matches a <style> or <script> opening tag (i.e.
// not the closing </…> form). The `[^>]*` swallows any other
// attributes; the final assertion checks the attribute set carries
// `nonce="<header-nonce>"`.
var inlineTagPattern = regexp.MustCompile(`<(style|script)\b[^>]*>`)

func assertEveryInlineTagCarriesNonce(t *testing.T, body, nonce string) {
	t.Helper()
	wantAttr := `nonce="` + nonce + `"`
	for _, m := range inlineTagPattern.FindAllStringIndex(body, -1) {
		tag := body[m[0]:m[1]]
		if !strings.Contains(tag, wantAttr) {
			t.Fatalf("inline tag missing matching nonce: %q (want attr %q)", tag, wantAttr)
		}
	}
}
