package csp

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

// helloHandler is the canonical handler used by the suite — it emits a
// short body and a 200 so the middleware's header-setting path is
// observable end-to-end.
var helloHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
})

func TestMiddleware_HeaderPresentAndPolicyShape(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(Middleware(helloHandler))
	defer srv.Close()

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if _, err := io.ReadAll(res.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}

	hdr := res.Header.Get(HeaderName)
	if hdr == "" {
		t.Fatalf("missing %s header", HeaderName)
	}

	required := []string{
		"default-src 'self'",
		"script-src 'self' 'nonce-",
		"style-src 'self' 'nonce-",
		"object-src 'none'",
		"base-uri 'self'",
		"frame-ancestors 'none'",
	}
	for _, want := range required {
		if !strings.Contains(hdr, want) {
			t.Errorf("CSP header missing %q\nfull header: %s", want, hdr)
		}
	}
	if strings.Contains(hdr, "unsafe-inline") {
		t.Errorf("CSP header must NOT contain 'unsafe-inline'; got: %s", hdr)
	}
	if strings.Contains(hdr, noncePlaceholder) {
		t.Errorf("CSP header leaks placeholder %q; got: %s", noncePlaceholder, hdr)
	}
}

// TestMiddleware_PerRequestUniqueNonces verifies that two consecutive
// requests yield distinct nonces. ADR 0077 §3.5 — every response gets a
// fresh nonce so a single leaked value cannot enable inline content for
// future requests.
func TestMiddleware_PerRequestUniqueNonces(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(Middleware(helloHandler))
	defer srv.Close()

	first, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	_, _ = io.ReadAll(first.Body)
	_ = first.Body.Close()
	second, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	_, _ = io.ReadAll(second.Body)
	_ = second.Body.Close()

	a := nonceFromHeader(t, first.Header.Get(HeaderName))
	b := nonceFromHeader(t, second.Header.Get(HeaderName))
	if a == "" || b == "" {
		t.Fatalf("missing nonce: a=%q b=%q", a, b)
	}
	if a == b {
		t.Errorf("expected distinct per-request nonces, got both = %q", a)
	}
}

// TestMiddleware_NonceLength_22Chars proves the nonce is the
// base64-url no-pad encoding of 16 random bytes. CSP3 §6.6.4 requires
// at least 128 bits (16 bytes); the encoding is 22 chars without pad.
func TestMiddleware_NonceLength_22Chars(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	Middleware(helloHandler).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	hdr := rec.Header().Get(HeaderName)
	got := nonceFromHeader(t, hdr)
	if l := len(got); l != 22 {
		t.Fatalf("nonce length = %d; want 22 (16 bytes base64url no-pad)\nheader: %s", l, hdr)
	}
}

// TestMiddleware_NonceFlowsToContext verifies the wrapped handler sees
// the same nonce as the header. Templates rely on this to emit `nonce`
// attributes that match the active policy.
func TestMiddleware_NonceFlowsToContext(t *testing.T) {
	t.Parallel()

	var fromCtx string
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		fromCtx = Nonce(r.Context())
	})
	rec := httptest.NewRecorder()
	Middleware(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	hdr := rec.Header().Get(HeaderName)
	want := nonceFromHeader(t, hdr)
	if fromCtx == "" || fromCtx != want {
		t.Fatalf("Nonce(ctx) = %q; header nonce = %q (want match)", fromCtx, want)
	}
}

// TestMiddleware_RandFailureReturns500 drives the entropy-source
// failure branch. crypto/rand failure is operationally rare, but the
// middleware must NOT invoke the wrapped handler in that state — the
// downstream handler would otherwise emit unprotected content.
func TestMiddleware_RandFailureReturns500(t *testing.T) {
	t.Parallel()

	called := false
	inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	})
	rec := httptest.NewRecorder()
	failing := iotest.ErrReader(errors.New("entropy device offline"))
	middlewareWith(inner, failing).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
	if called {
		t.Error("downstream handler must not be invoked on rand failure")
	}
	if got := rec.Header().Get(HeaderName); got != "" {
		t.Errorf("CSP header set on rand failure: %q (must be empty)", got)
	}
}

// TestMiddleware_RandShortReadFails covers the short-read branch — a
// reader that returns fewer than nonceBytes bytes must surface as an
// error (io.ReadFull, not io.Read).
func TestMiddleware_RandShortReadFails(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	short := bytes.NewReader([]byte{0x01, 0x02, 0x03}) // < nonceBytes
	middlewareWith(helloHandler, short).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500 on short read", rec.Code)
	}
}

func TestNonce_OutsideMiddlewareIsEmpty(t *testing.T) {
	t.Parallel()

	if got := Nonce(httptest.NewRequest(http.MethodGet, "/", nil).Context()); got != "" {
		t.Errorf("Nonce(plain ctx) = %q; want empty (fail-closed semantics)", got)
	}
}

// TestNonce_ProductionPathUsesCryptoRand spot-checks the production
// constructor (not the seam): we cannot verify entropy quality in a unit
// test, but we can verify the public Middleware path produces a
// well-formed header.
func TestMiddleware_ProductionConstructorEmitsValidHeader(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	Middleware(helloHandler).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	hdr := rec.Header().Get(HeaderName)
	if !strings.HasPrefix(hdr, "default-src 'self'") {
		t.Errorf("header malformed: %q", hdr)
	}
	got := nonceFromHeader(t, hdr)
	if len(got) != 22 {
		t.Errorf("nonce length = %d; want 22", len(got))
	}
}

// TestMiddleware_NonceUsesUnreservedURLChars asserts the base64-url
// charset; never `+`, `/`, or `=`. The CSP grammar accepts those, but a
// nonce that survives URL-encoding round trips and HTML attribute
// quoting without escaping is more robust.
func TestMiddleware_NonceUsesUnreservedURLChars(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	Middleware(helloHandler).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	got := nonceFromHeader(t, rec.Header().Get(HeaderName))
	for _, r := range got {
		ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			t.Errorf("nonce contains non-base64url char %q (full nonce: %q)", r, got)
		}
	}
}

// TestMiddleware_ContextCarriesSameNonceAsHeader is the
// integration-style proof that Nonce(ctx) and the emitted header are
// always in agreement — the contract templates rely on.
func TestMiddleware_ContextCarriesSameNonceAsHeader(t *testing.T) {
	t.Parallel()

	for i := 0; i < 25; i++ {
		var ctxNonce string
		inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			ctxNonce = Nonce(r.Context())
		})
		rec := httptest.NewRecorder()
		Middleware(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		hdrNonce := nonceFromHeader(t, rec.Header().Get(HeaderName))
		if ctxNonce != hdrNonce {
			t.Fatalf("iteration %d: ctx %q != header %q", i, ctxNonce, hdrNonce)
		}
	}
}

func TestMiddleware_RandReaderProductionMatches_internal(t *testing.T) {
	t.Parallel()

	// Sanity: the production constructor wires rand.Reader, not an
	// arbitrary io.Reader. We do not unwrap http.Handler internals; we
	// just make sure the production path does NOT 500 with the real
	// entropy source.
	rec := httptest.NewRecorder()
	Middleware(helloHandler).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("production middleware 500'd on the standard entropy source; status=%d", rec.Code)
	}
	// And reference rand.Reader so a future refactor that drops the
	// import surfaces as a compile error: rand.Reader is still the
	// production source of truth.
	_ = rand.Reader
}

// TestMiddleware_FontSrcAllowsSelfAndDataURIs asserts the SIN-63155
// directive: font-src must permit 'self' and data: so browser-extension
// UI that injects woff2 fonts as data: URIs does not pollute the console
// with CSP violation reports. data: must NOT leak into any other
// fetch-directive (default-src, script-src, style-src, img-src,
// object-src) — only font-src.
func TestMiddleware_FontSrcAllowsSelfAndDataURIs(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	Middleware(helloHandler).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	hdr := rec.Header().Get(HeaderName)
	if !strings.Contains(hdr, "font-src 'self' data:") {
		t.Errorf("CSP header missing %q\nfull header: %s", "font-src 'self' data:", hdr)
	}

	// data: must be scoped to font-src only. Walk each directive and
	// fail loudly if any other directive permits data:.
	for _, directive := range strings.Split(hdr, "; ") {
		if strings.HasPrefix(directive, "font-src") {
			continue
		}
		if strings.Contains(directive, "data:") {
			t.Errorf("data: leaked into directive %q (full header: %s)", directive, hdr)
		}
	}

	// Defence-in-depth: 'unsafe-inline' must remain banned everywhere
	// and default-src must stay locked to 'self'.
	if strings.Contains(hdr, "unsafe-inline") {
		t.Errorf("CSP header must NOT contain 'unsafe-inline'; got: %s", hdr)
	}
	if !strings.Contains(hdr, "default-src 'self';") {
		t.Errorf("default-src must remain 'self' only; got: %s", hdr)
	}
}

// nonceFromHeader extracts the substring after `'nonce-` from the
// script-src directive. Test helper — bails the test on a malformed
// header so callers don't have to thread the err.
func nonceFromHeader(t *testing.T, hdr string) string {
	t.Helper()
	const marker = "script-src 'self' 'nonce-"
	i := strings.Index(hdr, marker)
	if i < 0 {
		t.Fatalf("script-src directive not found in header: %q", hdr)
	}
	rest := hdr[i+len(marker):]
	end := strings.Index(rest, "'")
	if end < 0 {
		t.Fatalf("nonce closing quote not found: %q", rest)
	}
	return rest[:end]
}
