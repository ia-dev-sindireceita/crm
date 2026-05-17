package httpapi_test

// SIN-62978 — trusted-proxy-aware RealIP wrapper regression tests.
// The HIGH finding was that chimw.RealIP blindly rewrites r.RemoteAddr
// from True-Client-IP / X-Real-IP / X-Forwarded-For headers, letting a
// caller forge per-IP rate-limit bucket keys. The wrapper in
// internal/adapter/httpapi/trusted_realip.go gates that rewrite on the
// immediate TCP peer being inside the trusted-proxy CIDR allowlist.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
)

func TestNewTrustedRealIP_HonoursHeadersFromTrustedPeer(t *testing.T) {
	t.Parallel()
	// Default allowlist covers 127.0.0.1/32; httptest peers are
	// loopback so chi.RealIP rewrites are honoured.
	getenv := func(string) string { return "" }
	mw := httpapi.NewTrustedRealIP(getenv)

	var observed string
	handler := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		observed = r.RemoteAddr
	}))

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if observed != "203.0.113.7" {
		t.Fatalf("RemoteAddr = %q, want %q (rewrite honoured for trusted peer)", observed, "203.0.113.7")
	}
}

func TestNewTrustedRealIP_IgnoresHeadersFromUntrustedPeer(t *testing.T) {
	t.Parallel()
	// 198.51.100.0/24 is documented test range — outside any default
	// trusted CIDR. The wrapper MUST NOT rewrite r.RemoteAddr.
	getenv := func(string) string { return "" }
	mw := httpapi.NewTrustedRealIP(getenv)

	var observed string
	handler := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		observed = r.RemoteAddr
	}))

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.RemoteAddr = "198.51.100.10:55555"
	req.Header.Set("True-Client-IP", "1.2.3.4")
	req.Header.Set("X-Real-IP", "5.6.7.8")
	req.Header.Set("X-Forwarded-For", "9.10.11.12")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// chimw.RealIP NOT consulted → r.RemoteAddr stays as the raw peer.
	if observed != "198.51.100.10:55555" {
		t.Fatalf("RemoteAddr = %q, want %q (rewrite must be suppressed for untrusted peer)", observed, "198.51.100.10:55555")
	}
}

func TestNewTrustedRealIP_DropsIdentityHeadersForUntrustedPeer(t *testing.T) {
	t.Parallel()
	// The wrapper also strips the three identity headers so downstream
	// code that re-reads them via r.Header.Get cannot resurrect the
	// bypass. This is defence-in-depth — no shipped middleware reads
	// them today, but a future addition must not silently regress.
	getenv := func(string) string { return "" }
	mw := httpapi.NewTrustedRealIP(getenv)

	var got struct {
		trueClient, xreal, xff string
	}
	handler := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got.trueClient = r.Header.Get("True-Client-IP")
		got.xreal = r.Header.Get("X-Real-IP")
		got.xff = r.Header.Get("X-Forwarded-For")
	}))

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.RemoteAddr = "198.51.100.10:55555"
	req.Header.Set("True-Client-IP", "1.2.3.4")
	req.Header.Set("X-Real-IP", "5.6.7.8")
	req.Header.Set("X-Forwarded-For", "9.10.11.12")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got.trueClient != "" {
		t.Errorf("True-Client-IP not stripped: %q", got.trueClient)
	}
	if got.xreal != "" {
		t.Errorf("X-Real-IP not stripped: %q", got.xreal)
	}
	if got.xff != "" {
		t.Errorf("X-Forwarded-For not stripped: %q", got.xff)
	}
}

func TestNewTrustedRealIP_CustomCIDRsOverrideDefaults(t *testing.T) {
	t.Parallel()
	// Operator narrows the trust set: ONLY 192.0.2.0/24 trusted. The
	// default loopback range is no longer in the set, so a loopback
	// peer must not get its headers honoured even though they would by
	// default.
	getenv := func(name string) string {
		if name == httpapi.TrustedProxyEnv {
			return "192.0.2.0/24"
		}
		return ""
	}
	mw := httpapi.NewTrustedRealIP(getenv)

	var observed string
	handler := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		observed = r.RemoteAddr
	}))

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if observed != "127.0.0.1:55555" {
		t.Fatalf("RemoteAddr = %q, want %q (loopback no longer trusted)", observed, "127.0.0.1:55555")
	}

	// And a peer inside the explicit CIDR DOES get the rewrite.
	req2 := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req2.RemoteAddr = "192.0.2.5:55555"
	req2.Header.Set("X-Forwarded-For", "203.0.113.99")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if observed != "203.0.113.99" {
		t.Fatalf("RemoteAddr = %q, want %q (192.0.2.5 IS in the custom allowlist)", observed, "203.0.113.99")
	}
}

func TestNewTrustedRealIP_InvalidEnvFallsBackToDefaults(t *testing.T) {
	t.Parallel()
	// Operator typo: nothing parses → wrapper falls back to the
	// secure-by-default loopback + RFC1918 set instead of trusting
	// nothing (which would also be acceptable) OR trusting everything
	// (which would NOT be acceptable). Loopback peer is in the default
	// set so headers are honoured.
	getenv := func(name string) string {
		if name == httpapi.TrustedProxyEnv {
			return "not-a-cidr,,also-not-a-cidr"
		}
		return ""
	}
	mw := httpapi.NewTrustedRealIP(getenv)

	var observed string
	handler := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		observed = r.RemoteAddr
	}))

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if observed != "203.0.113.7" {
		t.Fatalf("RemoteAddr = %q, want %q (invalid env → defaults applied)", observed, "203.0.113.7")
	}
}

func TestNewTrustedRealIP_NilGetenvSafe(t *testing.T) {
	t.Parallel()
	// A nil getenv must not panic — fall back to defaults.
	mw := httpapi.NewTrustedRealIP(nil)

	var observed string
	handler := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		observed = r.RemoteAddr
	}))

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if observed != "203.0.113.7" {
		t.Fatalf("RemoteAddr = %q, want %q (nil getenv → defaults)", observed, "203.0.113.7")
	}
}

func TestNewTrustedRealIP_UnparseablePeerAddrIsUntrusted(t *testing.T) {
	t.Parallel()
	// Defensive: if the peer is unparseable (httptest oddity, raw
	// HTTP/2 peer, etc.) the wrapper treats it as untrusted and drops
	// the headers. Avoids the case where a malformed address slips
	// through into the rewrite path.
	getenv := func(string) string { return "" }
	mw := httpapi.NewTrustedRealIP(getenv)

	var observed string
	handler := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		observed = r.RemoteAddr
	}))

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.RemoteAddr = "not-an-ip-at-all"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if observed != "not-an-ip-at-all" {
		t.Fatalf("RemoteAddr = %q, want raw peer (unparseable → untrusted)", observed)
	}
}
