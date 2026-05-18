package httpapi

import (
	"log/slog"
	"net/http"
)

// NewTrustedRealIPWithLoggerForTest exposes the logger-injectable
// seam for trusted_realip_test so it can assert on the boot-time
// "trusted_proxy: dropped invalid CIDR entries" line without poking
// the process-global slog default (SIN-62985).
func NewTrustedRealIPWithLoggerForTest(getenv func(string) string, logger *slog.Logger) func(http.Handler) http.Handler {
	return newTrustedRealIPWithLogger(getenv, logger)
}

// ParseTrustedProxiesForTest exposes the internal parser so the test
// file can assert on the dropped-entries slice directly.
func ParseTrustedProxiesForTest(raw string) (cidrCount int, dropped []string) {
	out, dropped := parseTrustedProxies(raw)
	return len(out), dropped
}
