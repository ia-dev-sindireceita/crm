package customdomain

import (
	"context"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/pericles-luz/crm/internal/customdomain/management"
)

// VerifyRateLimiter is the per-(tenant, client IP) gate the verify
// middleware consults before letting a request reach the use-case.
// Allow consumes one token; on denial retryAfter is the wall-clock
// duration the client should wait before retrying.
//
// Defense-in-depth for SIN-63124: the public Verify endpoint hits DNS
// per request, so an unauthenticated burst from one (tenant, IP) pair
// would exhaust DNS budget and expose timing as a presence-enumeration
// side-channel. The bucket is per-(tenant, IP) so a NATed corporate
// office sharing one egress IP still gets a usable budget.
type VerifyRateLimiter interface {
	Allow(ctx context.Context, tenantID uuid.UUID, clientIP string) (allowed bool, retryAfter time.Duration)
}

// VerifyRateLimitConfig groups the construction-time knobs accepted by
// NewMemoryVerifyRateLimiter. Zero values use the SIN-63124 defaults
// (10 requests/minute per (tenant, IP) pair, burst 5, idle TTL 10
// minutes, real time). SweepInterval defaults to IdleTTL/2 so the
// janitor reclaims idle buckets at twice the eviction frequency.
type VerifyRateLimitConfig struct {
	Rate          rate.Limit
	Burst         int
	IdleTTL       time.Duration
	SweepInterval time.Duration
	Now           func() time.Time
}

// Defaults for VerifyRateLimitConfig — keep aligned with the SIN-63124
// acceptance criterion (10 req/min per (tenant, IP), burst 5) so the
// wire layer can NewMemoryVerifyRateLimiter(VerifyRateLimitConfig{})
// without spelling the numbers twice.
const (
	defaultVerifyRateLimitPerMin  = 10
	defaultVerifyRateLimitBurst   = 5
	defaultVerifyRateLimitIdleTTL = 10 * time.Minute
)

// DefaultVerifyRate is the production token-bucket refill rate: 10
// requests per 60 seconds, expressed as tokens-per-second so it slots
// directly into rate.NewLimiter.
var DefaultVerifyRate = rate.Limit(float64(defaultVerifyRateLimitPerMin) / 60.0)

type bucket struct {
	lim      *rate.Limiter
	lastSeen atomic.Int64 // unix nanos; written under m.mu, read lock-free
}

// MemoryVerifyRateLimiter is the in-process token-bucket implementation
// backing VerifyRateLimiter. It is defense-in-depth only — the limiter
// does NOT coordinate across server instances, so a deployment that
// scales the API beyond a single pod must layer a Redis-backed
// limiter on top (out of scope for SIN-63124).
type MemoryVerifyRateLimiter struct {
	rate          rate.Limit
	burst         int
	idleTTL       time.Duration
	sweepInterval time.Duration
	now           func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

// NewMemoryVerifyRateLimiter returns a limiter ready to be wired into
// VerifyRateLimitMiddleware. Safe for concurrent use.
func NewMemoryVerifyRateLimiter(cfg VerifyRateLimitConfig) *MemoryVerifyRateLimiter {
	r := cfg.Rate
	if r <= 0 {
		r = DefaultVerifyRate
	}
	b := cfg.Burst
	if b <= 0 {
		b = defaultVerifyRateLimitBurst
	}
	ttl := cfg.IdleTTL
	if ttl <= 0 {
		ttl = defaultVerifyRateLimitIdleTTL
	}
	sweep := cfg.SweepInterval
	if sweep <= 0 {
		sweep = ttl / 2
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &MemoryVerifyRateLimiter{
		rate:          r,
		burst:         b,
		idleTTL:       ttl,
		sweepInterval: sweep,
		now:           now,
		buckets:       make(map[string]*bucket),
	}
}

// SweepInterval reports the cadence the janitor uses between Sweep
// calls. Exposed so the wire layer can log the effective cadence at
// boot without re-reading the env-driven config.
func (m *MemoryVerifyRateLimiter) SweepInterval() time.Duration {
	return m.sweepInterval
}

// RunJanitor blocks until ctx is cancelled, calling Sweep on
// SweepInterval. Returns ctx.Err() on exit so the caller can
// distinguish graceful shutdown from a programming error. Designed to
// be spawned in its own goroutine from the wire layer alongside the
// HTTP server's shutdown context, so SIGTERM cancels the janitor
// without leaking the goroutine.
func (m *MemoryVerifyRateLimiter) RunJanitor(ctx context.Context) error {
	t := time.NewTicker(m.sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			m.Sweep()
		}
	}
}

func limiterKey(tenant uuid.UUID, ip string) string {
	return tenant.String() + "|" + ip
}

func (m *MemoryVerifyRateLimiter) lookup(key string) *rate.Limiter {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(m.rate, m.burst)}
		m.buckets[key] = b
	}
	b.lastSeen.Store(m.now().UnixNano())
	return b.lim
}

// Allow consumes one token from the (tenant, IP) bucket. nil/empty
// inputs are refused with a conservative 1s retry so a request that
// somehow bypassed tenant resolution cannot bypass the limiter.
func (m *MemoryVerifyRateLimiter) Allow(_ context.Context, tenantID uuid.UUID, clientIP string) (bool, time.Duration) {
	if tenantID == uuid.Nil || clientIP == "" {
		return false, time.Second
	}
	lim := m.lookup(limiterKey(tenantID, clientIP))
	now := m.now()
	r := lim.ReserveN(now, 1)
	if !r.OK() {
		return false, time.Second
	}
	delay := r.DelayFrom(now)
	if delay > 0 {
		r.CancelAt(now)
		return false, delay
	}
	return true, 0
}

// Sweep deletes buckets whose lastSeen is older than IdleTTL. Returns
// the count removed. Safe for concurrent use. Production wires a
// janitor goroutine that calls Sweep periodically; tests call it
// directly to assert eviction semantics.
func (m *MemoryVerifyRateLimiter) Sweep() int {
	cutoff := m.now().Add(-m.idleTTL).UnixNano()
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := 0
	for k, b := range m.buckets {
		if b.lastSeen.Load() < cutoff {
			delete(m.buckets, k)
			removed++
		}
	}
	return removed
}

// Len reports the number of live buckets. Test-only helper.
func (m *MemoryVerifyRateLimiter) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.buckets)
}

// VerifyRateLimitMiddleware returns an http.Handler middleware that
// gates the wrapped handler on lim. On allow it passes through; on
// deny it writes 429 with a Retry-After header (whole seconds, rounded
// up) and a short text/plain body, and emits a denied:rate_limited
// audit event so IR can correlate abuse against the dns_resolution_log
// table.
//
// nil lim disables the middleware entirely (returns next unchanged) so
// tests that do not exercise rate-limiting need not construct a
// limiter. nil audit silently drops the audit emit on denial; the
// 429 is still written. now defaults to time.Now; logger defaults to
// slog.Default.
func VerifyRateLimitMiddleware(lim VerifyRateLimiter, audit management.AuditLogger, now func() time.Time, logger *slog.Logger) func(http.Handler) http.Handler {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		if lim == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant := TenantIDFromContext(r.Context())
			if tenant == uuid.Nil {
				http.Error(w, "Sessão de tenant não encontrada.", http.StatusUnauthorized)
				return
			}
			ip := clientIP(r)
			allowed, retry := lim.Allow(r.Context(), tenant, ip)
			if allowed {
				next.ServeHTTP(w, r)
				return
			}
			if retry <= 0 {
				retry = time.Second
			}
			secs := int(math.Ceil(retry.Seconds()))
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("Muitas tentativas de verificação. Aguarde alguns segundos e tente novamente."))

			if audit != nil {
				audit.LogManagement(r.Context(), management.AuditEvent{
					TenantID: tenant,
					DomainID: parseIDForAudit(r),
					Action:   "verify",
					Outcome:  "denied:rate_limited",
					Reason:   management.ReasonRateLimited,
					At:       now(),
				})
			}
			logger.LogAttrs(r.Context(), slog.LevelInfo, "customdomain.verify.rate_limited",
				slog.String("tenant", tenant.String()),
				slog.String("client_ip", ip),
				slog.Int("retry_after_seconds", secs),
			)
		})
	}
}

// clientIP returns the peer address with the port stripped. We rely on
// httpapi.NewTrustedRealIP (SIN-62978) to have already canonicalised
// RemoteAddr to the actual client behind trusted proxies before this
// middleware runs.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// parseIDForAudit best-efforts the {id} path parameter; uuid.Nil on
// parse failure. The audit event still carries the tenant + IP through
// the structured slog line, so an invalid-UUID probe is observable
// even when DomainID is empty.
func parseIDForAudit(r *http.Request) uuid.UUID {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return uuid.Nil
	}
	return id
}
