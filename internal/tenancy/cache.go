package tenancy

import (
	"context"
	"errors"
	"sync"
	"time"
)

// DefaultCacheTTL is the per-host TTL applied when callers do not supply
// one. 5 minutes is short enough that custom-domain renames or tenant
// host edits propagate quickly without forcing a redeploy.
const DefaultCacheTTL = 5 * time.Minute

// CachingResolver wraps an upstream Resolver with a per-host TTL cache.
// Negative results (ErrTenantNotFound) are cached too, so a flood of
// requests to a bogus host does not hammer the database.
//
// The implementation uses a single mutex; tenant resolution happens
// once per request and the cache lives in front of a network round-trip
// — the lock contention is not measurable. If that ever changes, swap
// the map for sync.Map without changing the public surface.
type CachingResolver struct {
	upstream Resolver
	ttl      time.Duration
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	tenant    *Tenant
	err       error
	expiresAt time.Time
}

// NewCachingResolver wraps upstream with a TTL cache. ttl ≤ 0 falls
// back to DefaultCacheTTL so callers cannot accidentally disable
// caching by passing the zero value.
func NewCachingResolver(upstream Resolver, ttl time.Duration) *CachingResolver {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &CachingResolver{
		upstream: upstream,
		ttl:      ttl,
		now:      time.Now,
		entries:  make(map[string]cacheEntry),
	}
}

// ResolveByHost serves the cached entry when fresh; otherwise it calls
// the upstream resolver and caches the result (positive or negative).
// ErrTenantNotFound is the only error cached — anything else is
// transient and would poison the cache.
func (c *CachingResolver) ResolveByHost(ctx context.Context, host string) (*Tenant, error) {
	if c == nil || c.upstream == nil {
		return nil, errors.New("tenancy: nil caching resolver")
	}

	c.mu.Lock()
	if entry, ok := c.entries[host]; ok && c.now().Before(entry.expiresAt) {
		c.mu.Unlock()
		return entry.tenant, entry.err
	}
	c.mu.Unlock()

	tenant, err := c.upstream.ResolveByHost(ctx, host)
	if err != nil && !errors.Is(err, ErrTenantNotFound) {
		// Don't cache transient errors — let the next request try again.
		return nil, err
	}

	c.mu.Lock()
	c.entries[host] = cacheEntry{
		tenant:    tenant,
		err:       err,
		expiresAt: c.now().Add(c.ttl),
	}
	c.mu.Unlock()
	return tenant, err
}

// Invalidate drops a single host from the cache. Useful for tests and
// for the eventual admin endpoint that renames a tenant's host.
func (c *CachingResolver) Invalidate(host string) {
	c.mu.Lock()
	delete(c.entries, host)
	c.mu.Unlock()
}
