package webchat

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClientIP_IgnoresSpoofedForwardHeaders is the SIN-64991 regression:
// clientIP() is the rate-limit + ip_hash key source. It must trust ONLY
// r.RemoteAddr (already canonicalised by the router-root trusted-proxy
// RealIP wrapper, httpapi.NewTrustedRealIP / SIN-62978) and never read
// the raw X-Real-IP / X-Forwarded-For / True-Client-IP headers. If it
// did, a caller not behind the Caddy edge could spoof the header
// per-request to partition the per-IP session rate-limit bucket (D5
// bypass, OWASP A05).
func TestClientIP_IgnoresSpoofedForwardHeaders(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       string
	}{
		{
			name:       "spoofed X-Real-IP ignored",
			remoteAddr: "203.0.113.7:5555",
			headers:    map[string]string{"X-Real-IP": "10.9.9.9"},
			want:       "203.0.113.7",
		},
		{
			name:       "spoofed X-Forwarded-For ignored",
			remoteAddr: "203.0.113.7:5555",
			headers:    map[string]string{"X-Forwarded-For": "10.9.9.9, 8.8.8.8"},
			want:       "203.0.113.7",
		},
		{
			name:       "spoofed True-Client-IP ignored",
			remoteAddr: "203.0.113.7:5555",
			headers:    map[string]string{"True-Client-IP": "10.9.9.9"},
			want:       "203.0.113.7",
		},
		{
			name:       "bare IPv6 RemoteAddr not truncated at last colon",
			remoteAddr: "2001:db8::1",
			want:       "2001:db8::1",
		},
		{
			name:       "bracketed IPv6 with port strips port",
			remoteAddr: "[2001:db8::1]:5555",
			want:       "2001:db8::1",
		},
		{
			name:       "bare IPv4 RemoteAddr returned unchanged",
			remoteAddr: "203.0.113.7",
			want:       "203.0.113.7",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/widget/v1/stream", nil)
			r.RemoteAddr = c.remoteAddr
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			if got := clientIP(r); got != c.want {
				t.Fatalf("clientIP() = %q, want %q (RemoteAddr=%q, headers=%v)",
					got, c.want, c.remoteAddr, c.headers)
			}
		})
	}
}

// TestClientIP_SpoofedHeaderDoesNotPartitionBucket asserts the
// rate-limit-relevant property directly: two requests from the same TCP
// peer but with different spoofed X-Real-IP values resolve to the SAME
// clientIP, so they share one rate-limit / ip_hash bucket. Pre-fix the
// attacker could rotate X-Real-IP to get a fresh bucket per request.
func TestClientIP_SpoofedHeaderDoesNotPartitionBucket(t *testing.T) {
	mk := func(spoof string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/widget/v1/stream", nil)
		r.RemoteAddr = "198.51.100.42:4444"
		r.Header.Set("X-Real-IP", spoof)
		return r
	}
	a := clientIP(mk("10.0.0.1"))
	b := clientIP(mk("10.0.0.2"))
	if a != b {
		t.Fatalf("spoofed X-Real-IP partitioned the bucket: %q != %q", a, b)
	}
	if a != "198.51.100.42" {
		t.Fatalf("clientIP keyed off spoofed header, got %q want real peer 198.51.100.42", a)
	}
}
