// Package ratelimit holds the HTTP middleware that enforces the
// per-bucket rate limits defined in iam/ratelimit policies (SIN-62341,
// ADR 0073 §D4).
//
// The middleware shape is the standard `func(http.Handler) http.Handler`
// so it composes with both stdlib mux and chi without extra glue.
//
// Scope of this PR: per-bucket pre-check and 429 + Retry-After
// rendering. Wiring into the login handler — including the failure-
// counter increment and account_lockout writes — lives in the login
// integration PR (SIN-62341 §"pode vir em 2 PRs"); the Lockouts port
// + adapter shipped here is what that PR consumes.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	domainratelimit "github.com/pericles-luz/crm/internal/iam/ratelimit"
)

// KeyExtractor pulls one request-scoped value (IP, hashed email,
// session id, user id) from the request. Extractors that return ""
// signal "no key available for this bucket" — the middleware skips
// the bucket rather than 429-ing on a missing extractor (e.g. a
// pre-login route that has no session id yet for the session bucket).
type KeyExtractor func(r *http.Request) string

// Bucket binds a Policy bucket name to the extractor that fills it.
// The slice order is the bucket-evaluation order: the FIRST throttled
// bucket short-circuits with a 429.
type Bucket struct {
	Name      string
	Extractor KeyExtractor
}

// Config wires the policy and per-bucket extractors together.
//
//   - Policy is the precomputed iam/ratelimit.Policy for the route
//     (e.g. policies["login"]).
//   - Limiter is the shared RateLimiter implementation (typically
//     redis.SlidingWindow).
//   - Buckets supplies one Extractor per bucket the policy declares.
//     A missing extractor for a declared bucket is a programmer
//     error: New returns an error so the wireup site fails at startup
//     rather than silently letting a bucket be skipped on every
//     request.
//   - OnDeny (optional) is invoked on every 429. Production wires this
//     to a metrics counter; tests inspect the recorded calls.
//   - Logger (optional, default slog.Default) records throttled and
//     limiter-error events.
type Config struct {
	Policy  domainratelimit.Policy
	Limiter domainratelimit.RateLimiter
	Buckets []Bucket
	OnDeny  func(policy, bucket, key string, retryAfter time.Duration)
	Logger  *slog.Logger
}

// New builds a chi-compatible middleware enforcing cfg.Policy. It
// validates that every bucket in cfg.Policy has a matching entry in
// cfg.Buckets so a config drift surfaces at startup.
func New(cfg Config) (func(http.Handler) http.Handler, error) {
	if cfg.Limiter == nil {
		return nil, errors.New("httpapi/ratelimit: nil Limiter")
	}
	if len(cfg.Policy.Buckets) == 0 {
		return nil, fmt.Errorf("httpapi/ratelimit: policy %q has no buckets", cfg.Policy.Name)
	}
	if len(cfg.Buckets) == 0 {
		return nil, fmt.Errorf("httpapi/ratelimit: policy %q has no extractors wired", cfg.Policy.Name)
	}

	// Build a fast lookup: bucket name → policy bucket. Then verify
	// each cfg.Buckets entry resolves to a known bucket.
	policyByName := make(map[string]domainratelimit.Bucket, len(cfg.Policy.Buckets))
	for _, b := range cfg.Policy.Buckets {
		policyByName[b.Name] = b
	}
	wired := make(map[string]struct{}, len(cfg.Buckets))
	type pair struct {
		policy    domainratelimit.Bucket
		extractor KeyExtractor
	}
	pairs := make([]pair, 0, len(cfg.Buckets))
	for _, b := range cfg.Buckets {
		pb, ok := policyByName[b.Name]
		if !ok {
			return nil, fmt.Errorf("httpapi/ratelimit: policy %q does not declare bucket %q", cfg.Policy.Name, b.Name)
		}
		if b.Extractor == nil {
			return nil, fmt.Errorf("httpapi/ratelimit: policy %q bucket %q has nil extractor", cfg.Policy.Name, b.Name)
		}
		if _, dup := wired[b.Name]; dup {
			return nil, fmt.Errorf("httpapi/ratelimit: policy %q bucket %q wired twice", cfg.Policy.Name, b.Name)
		}
		wired[b.Name] = struct{}{}
		pairs = append(pairs, pair{policy: pb, extractor: b.Extractor})
	}
	for _, b := range cfg.Policy.Buckets {
		if _, ok := wired[b.Name]; !ok {
			return nil, fmt.Errorf("httpapi/ratelimit: policy %q bucket %q has no extractor", cfg.Policy.Name, b.Name)
		}
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	policyName := cfg.Policy.Name
	limiter := cfg.Limiter
	onDeny := cfg.OnDeny

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			for _, p := range pairs {
				key := p.extractor(r)
				if key == "" {
					// Bucket has no key for this request — skip it.
					// The caller decided this extractor is optional
					// (e.g. session-id bucket on a pre-session route).
					continue
				}
				fullKey := policyName + ":" + p.policy.Name + ":" + key
				allowed, retryAfter, err := limiter.Allow(ctx, fullKey, p.policy.Window, p.policy.Max)
				if err != nil {
					// Fail-open on infra error: the security review of
					// SIN-62220 calls out that a Redis outage MUST NOT
					// take the auth path down with it (defense in
					// depth — Postgres lockout still applies). Log loud
					// so ops sees it.
					logger.WarnContext(ctx, "ratelimit: limiter error — failing open",
						slog.String("policy", policyName),
						slog.String("bucket", p.policy.Name),
						slog.String("err", err.Error()),
					)
					continue
				}
				if !allowed {
					if onDeny != nil {
						onDeny(policyName, p.policy.Name, key, retryAfter)
					}
					logger.InfoContext(ctx, "ratelimit: throttled",
						slog.String("policy", policyName),
						slog.String("bucket", p.policy.Name),
						slog.Duration("retry_after", retryAfter),
					)
					writeRateLimited(w, retryAfter)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// writeRateLimited renders the 429 response with a Retry-After header
// rounded UP to the next whole second (RFC 7231 mandates a delta-seconds
// integer). A retryAfter of 0 still emits Retry-After: 1 so clients
// always see a positive value.
func writeRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int64(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte("rate limit exceeded\n"))
}

// IPKeyExtractor returns the request remote address with the port
// stripped. It is the canonical extractor for the per-IP buckets.
//
// IMPORTANT — production reverse-proxy wiring. When the server sits
// behind Caddy or a load balancer the trustworthy client IP is in
// X-Forwarded-For, not in r.RemoteAddr. The trusted-proxy parsing
// belongs to the http server bootstrap (Caddy strips/sets this header
// per ADR 0070 §edge), which then writes the canonical IP into
// r.RemoteAddr. This extractor stays naive on purpose so the trust
// boundary lives in one place; tests that need to simulate a proxy
// set RemoteAddr directly.
func IPKeyExtractor(r *http.Request) string {
	if r == nil {
		return ""
	}
	addr := r.RemoteAddr
	if addr == "" {
		return ""
	}
	// Try the canonical "host:port" or "[v6]:port" forms first; on
	// success the host alone is what we want. SplitHostPort is strict
	// — bare IPs (no port) and unbracketed v6 fall through to the
	// "treat as a literal address" branch.
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// FormFieldExtractor returns an extractor that pulls a single form
// field by name. It calls r.ParseForm so the field is read regardless
// of GET vs POST encoding. ParseForm errors are absorbed (no key →
// skip bucket) so a malformed body does not crash the limiter; the
// downstream handler will reject the request on its own merits.
func FormFieldExtractor(name string) KeyExtractor {
	return func(r *http.Request) string {
		if r == nil {
			return ""
		}
		_ = r.ParseForm()
		return r.PostFormValue(name)
	}
}

// ContextStringExtractor returns an extractor that reads a string
// value from the request context (e.g. a session id put there by an
// upstream session middleware). missing or wrong-type → empty key.
func ContextStringExtractor(key any) KeyExtractor {
	return func(r *http.Request) string {
		if r == nil {
			return ""
		}
		v, _ := contextValueString(r.Context(), key)
		return v
	}
}

func contextValueString(ctx context.Context, key any) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v := ctx.Value(key)
	s, ok := v.(string)
	return s, ok
}
