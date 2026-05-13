package mastermfa

import (
	"net"
	"net/http"
)

// clientIP returns the request's client IP as a string, stripping
// the port off r.RemoteAddr when present. Returns "" when the
// address is empty or unparseable so the alert body shows a clean
// blank rather than "<nil>". The trusted-proxy / X-Forwarded-For
// rewrite is the responsibility of an upstream middleware (mirrors
// what loginhandler and ratelimit already do): this helper stays
// naive on purpose.
func clientIP(r *http.Request) string {
	addr := r.RemoteAddr
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
