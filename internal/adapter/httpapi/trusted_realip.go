package httpapi

// SIN-62978 — trusted-proxy-aware wrapper around chi's RealIP middleware
// (HIGH security finding, AC #1 of SIN-62978, follow-up of SIN-62959
// AC #7). The package-level doc-comment in router.go documents the
// middleware chain; this file owns the wrapper that decides whether to
// honour the client-supplied X-Forwarded-For / X-Real-IP / True-Client-IP
// headers on the current request.
//
// Threat model. chi v5 middleware/realip.go rewrites r.RemoteAddr from
// the first non-empty value of {True-Client-IP, X-Real-IP, first token
// of X-Forwarded-For}. If those headers reach the app from an untrusted
// caller, the per-IP rate-limit buckets keyed off r.RemoteAddr (see
// internal/adapter/httpapi/ratelimit/middleware.go:IPKeyExtractor) become
// attacker-controlled and the AC #4 100/min/IP cap on GET /c/{slug}
// collapses. The edge (Caddy) is now configured to strip the three
// headers (deploy/caddy/Caddyfile + Caddyfile.stg) — this wrapper is the
// belt-and-braces defence against operator misconfig: if the headers
// somehow arrive on the app socket (direct test, regressed Caddy config,
// internal listener bypassed), the wrapper falls through to the raw TCP
// peer address instead of trusting the attacker's value.
//
// Trust gate. The immediate TCP peer (r.RemoteAddr before any rewrite)
// MUST be in the trusted-proxy CIDR allowlist for the headers to be
// honoured. The allowlist defaults to the docker-internal ranges that
// production uses (127.0.0.1/32, 172.16.0.0/12, 10.0.0.0/8) and is
// overridable via TRUSTED_PROXY_CIDRS at process start so an operator
// running behind a different reverse proxy can tune it without a rebuild.

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// defaultTrustedProxyCIDRs is the production-safe baseline: loopback +
// the two RFC1918 ranges docker compose uses for its bridge networks
// (172.16/12) and that operators commonly assign to internal L4 LBs
// (10/8). Anything outside this set is treated as untrusted unless the
// operator overrides TRUSTED_PROXY_CIDRS at boot.
//
// Note that the public docker bridge `bridge0` is 172.17.0.0/16; the
// app's user-defined network in deploy/compose/compose.yml falls inside
// 172.16/12. The Unbound + Caddy services share that network so the
// caddy → app TCP peer always lands inside the allowlist for the
// happy path.
var defaultTrustedProxyCIDRs = []string{
	"127.0.0.1/32",
	"::1/128",
	"172.16.0.0/12",
	"10.0.0.0/8",
}

// TrustedProxyEnv is the env var name that overrides the default
// trusted-proxy CIDR list. Format: comma-separated CIDRs (e.g.
// "10.0.0.0/8,192.168.1.0/24"). Empty / unset → defaults. Invalid
// entries are silently dropped from the active set and aggregated
// into a single info-level log line emitted by NewTrustedRealIP at
// boot (`msg="trusted_proxy: dropped invalid CIDR entries"`,
// `dropped` carries the raw values, `fellback` is true when every
// entry was invalid and the wrapper fell back to defaultTrustedProxyCIDRs).
// The remaining valid CIDRs still apply. Operators running the
// staging runbook's "safe degrade" check (`docs/deploy/staging.md`
// § HTTP edge) should grep boot logs for `trusted_proxy: dropped`.
const TrustedProxyEnv = "TRUSTED_PROXY_CIDRS"

// trustedRealIP returns a middleware that:
//
//  1. inspects the immediate TCP peer (r.RemoteAddr) BEFORE any rewrite;
//  2. if the peer IP is inside one of trusted, delegates to chimw.RealIP
//     which honours the client-supplied identity headers;
//  3. otherwise serves the inner handler with r.RemoteAddr untouched,
//     so the per-IP rate-limit middleware downstream sees the actual
//     attacker IP rather than a forged value.
//
// An empty trusted slice disables the RealIP rewrite for every request
// — equivalent to never mounting chimw.RealIP — which is the safe
// failure mode if env parsing collapses entirely.
//
// The wrapper is intentionally cheap: one IP parse + a linear walk over
// trusted CIDRs per request. The trusted set is small (3–5 entries in
// production), so allocating an *net.IPNet per request would dwarf the
// actual work — instead, NewTrustedRealIP pre-parses the slice once at
// composition root.
func trustedRealIP(trusted []*net.IPNet) func(http.Handler) http.Handler {
	rewrite := chimw.RealIP
	return func(next http.Handler) http.Handler {
		rewritten := rewrite(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			peer := parsePeerIP(r.RemoteAddr)
			if peer == nil || !ipIn(peer, trusted) {
				// Untrusted peer (or unparseable). Drop the
				// client-supplied identity headers from the
				// request so downstream middleware that re-reads
				// them via r.Header.Get("X-Forwarded-For") (none
				// today, but defence-in-depth for future code)
				// cannot resurrect the bypass; r.RemoteAddr is
				// already the raw peer.
				stripIdentityHeaders(r)
				next.ServeHTTP(w, r)
				return
			}
			rewritten.ServeHTTP(w, r)
		})
	}
}

// NewTrustedRealIP is the composition-root constructor used by NewRouter
// and exported for cmd/server tests that want to assert against the
// wrapper directly. The env getter is parametric so unit tests can pin
// the allowlist without poking process state.
//
// On any parse failure the wrapper falls back to the documented
// defaults (loopback + RFC1918) so a misconfigured env var degrades to
// the secure-by-default posture instead of opening the bypass. Each
// dropped raw entry is surfaced once via slog.Default at info level so
// operators following the staging runbook's "safe degrade" check have
// a discoverable boot-time signal (SIN-62985).
func NewTrustedRealIP(getenv func(string) string) func(http.Handler) http.Handler {
	return newTrustedRealIPWithLogger(getenv, slog.Default())
}

// newTrustedRealIPWithLogger is the test seam used by trusted_realip_test
// to capture the info-level drop notification without poking the
// process-global slog default. Production code calls NewTrustedRealIP
// which delegates here with slog.Default().
func newTrustedRealIPWithLogger(getenv func(string) string, logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	raw := envOrDefault(getenv, TrustedProxyEnv, "")
	trusted, dropped := parseTrustedProxies(raw)
	fellback := false
	if len(trusted) == 0 {
		// Every entry was invalid (or the env was unset). Fall back to
		// the secure default set; the default list is parsed by the
		// same helper to keep the code path uniform — its dropped
		// slice is empty by construction.
		trusted, _ = parseTrustedProxies(strings.Join(defaultTrustedProxyCIDRs, ","))
		fellback = raw != "" // only true when an operator explicitly set the env
	}
	if len(dropped) > 0 {
		// Aggregate, one line per boot. The structured fields are
		// stable (dropped, fellback) so a Loki / Grafana alert can
		// match without parsing the human-readable msg.
		logger.LogAttrs(context.Background(), slog.LevelInfo,
			"trusted_proxy: dropped invalid CIDR entries",
			slog.String("env", TrustedProxyEnv),
			slog.String("dropped", strings.Join(dropped, ",")),
			slog.Int("dropped_count", len(dropped)),
			slog.Bool("fellback", fellback),
		)
	}
	return trustedRealIP(trusted)
}

// parseTrustedProxies splits a comma-separated CIDR list, trims each
// entry, parses it via net.ParseCIDR, and returns:
//
//   - out: the slice of *IPNet that parsed successfully.
//   - dropped: the trimmed raw values that failed to parse, in input
//     order. Empty entries (trailing comma, "a,,b") are skipped
//     silently — they cost nothing semantically and an operator who
//     pasted them does not need a log line.
//
// The caller (NewTrustedRealIP) falls back to defaults when out is
// empty AND logs dropped via slog at info level so operators see the
// drop without having to enable debug logging.
func parseTrustedProxies(raw string) (out []*net.IPNet, dropped []string) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out = make([]*net.IPNet, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}
		_, cidr, err := net.ParseCIDR(s)
		if err != nil || cidr == nil {
			dropped = append(dropped, s)
			continue
		}
		out = append(out, cidr)
	}
	return out, dropped
}

// parsePeerIP extracts the IP portion of a remoteAddr like
// "192.168.1.1:5555" or "[::1]:5555" or a bare "192.168.1.1". Returns
// nil when the address is unparseable so callers can route to the
// untrusted branch.
func parsePeerIP(remoteAddr string) net.IP {
	if remoteAddr == "" {
		return nil
	}
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	return net.ParseIP(host)
}

// ipIn reports whether ip is contained in any of the cidrs. A nil ip or
// empty cidrs slice yields false. Walks linearly — the trusted set is a
// handful of entries.
func ipIn(ip net.IP, cidrs []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, cidr := range cidrs {
		if cidr != nil && cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// stripIdentityHeaders removes the three headers chi.RealIP consults.
// Used on the untrusted-peer branch so any downstream middleware that
// re-reads them (none in the current tree, but defence-in-depth for
// future code) cannot resurrect the bypass. r.Header is mutated in
// place; net/http handlers commonly read headers through Get which is
// case-insensitive, so the canonicalised names suffice.
func stripIdentityHeaders(r *http.Request) {
	r.Header.Del("True-Client-IP")
	r.Header.Del("X-Real-IP")
	r.Header.Del("X-Forwarded-For")
}

// envOrDefault returns the value of name from getenv, or fallback when
// getenv returns the empty string. A nil getenv yields fallback to keep
// the call site short.
func envOrDefault(getenv func(string) string, name, fallback string) string {
	if getenv == nil {
		return fallback
	}
	v := getenv(name)
	if v == "" {
		return fallback
	}
	return v
}
