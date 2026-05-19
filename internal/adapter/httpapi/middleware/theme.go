package middleware

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// DefaultThemeCacheTTL is the per-tenant palette cache TTL applied
// when ThemeConfig.TTL is zero. 60 seconds matches the spec
// (SIN-63085 §Scope): short enough that a SIN-63084 palette save
// reflects to peer shards within one minute without explicit
// invalidation, long enough to keep cache hit ratio > 95% under
// realistic traffic (one miss per minute per tenant, every other
// request a hit).
const DefaultThemeCacheTTL = 60 * time.Second

// ThemeCacheResult labels used on the per-lookup metric. Closed enum
// so the registry stays low-cardinality.
const (
	ThemeCacheResultHit      = "hit"
	ThemeCacheResultMiss     = "miss"
	ThemeCacheResultNoTenant = "no_tenant"
	ThemeCacheResultError    = "error"
)

// ThemeMetrics is the subset of obs.Metrics the theme middleware
// reaches for. The interface lets tests inject a counter recorder
// without depending on the full Metrics surface and lets pre-SIN-63085
// callers wire the middleware with nil metrics without a panic.
type ThemeMetrics interface {
	ObserveThemeCacheLookup(result string)
}

// ThemeConfig collects the dependencies of the Theme middleware.
//
// Store is optional: nil means "no per-tenant overrides", every
// request gets DefaultThemeStyle. Tests and pre-SIN-63075 deploys can
// rely on this to mount the middleware without a database adapter.
//
// Metrics is optional too — a nil value disables the per-result
// counter and is the right wiring for tests that don't assert
// observability.
type ThemeConfig struct {
	Store   branding.PaletteStore
	TTL     time.Duration
	Now     func() time.Time
	Metrics ThemeMetrics
}

// ThemeMiddleware is the constructed bundle exposed by NewTheme.
// Handler is the chi/net-http wrapper; Invalidate evicts a cached
// entry so the SIN-63084 palette-save handler can guarantee the next
// request renders the freshly persisted tokens (AC #4 of SIN-63085).
type ThemeMiddleware struct {
	cache   *themeCache
	store   branding.PaletteStore
	metrics ThemeMetrics
}

// NewTheme constructs the middleware bundle. The returned value is
// reusable: the same instance backs the chi handler chain and the
// SIN-63084 invalidation seam.
//
// The middleware MUST be mounted AFTER middleware.TenantScope so the
// resolved tenant is available on the context. Missing-tenant requests
// fall through to DefaultThemeStyle and are recorded with the
// "no_tenant" result label.
func NewTheme(cfg ThemeConfig) *ThemeMiddleware {
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultThemeCacheTTL
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &ThemeMiddleware{
		cache: &themeCache{
			ttl:     ttl,
			now:     now,
			entries: make(map[uuid.UUID]themeCacheEntry),
		},
		store:   cfg.Store,
		metrics: cfg.Metrics,
	}
}

// Handler is the chi-compatible middleware function. It looks up the
// per-tenant palette (cache first, then store) and attaches the
// rendered :root{...} style to the request context. Downstream
// handlers retrieve it via branding.ThemeStyleFromContext.
func (tm *ThemeMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		style := tm.lookup(r.Context())
		ctx := branding.WithThemeStyle(r.Context(), style)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Invalidate evicts the cached entry for tenantID. Returns true when
// an entry was present and removed, false otherwise — callers can use
// the boolean to telemetry the SIN-63084 invalidation hit rate. Safe
// to call with a nil receiver so a partially-wired SIN-63084 handler
// is a no-op rather than a crash.
func (tm *ThemeMiddleware) Invalidate(tenantID uuid.UUID) bool {
	if tm == nil {
		return false
	}
	return tm.cache.invalidate(tenantID)
}

// lookup resolves the inline style for the request's tenant.
// Branches map 1:1 to the metric labels above.
func (tm *ThemeMiddleware) lookup(ctx context.Context) template.CSS {
	tenant, err := tenancy.FromContext(ctx)
	if err != nil {
		tm.observe(ThemeCacheResultNoTenant)
		return branding.DefaultThemeStyle
	}

	if style, ok := tm.cache.get(tenant.ID); ok {
		tm.observe(ThemeCacheResultHit)
		return style
	}

	style := branding.DefaultThemeStyle
	if tm.store != nil {
		palette, err := tm.store.GetByTenantID(ctx, tenant.ID)
		switch {
		case err == nil:
			style = branding.ThemeStyleFromPalette(palette)
		case errors.Is(err, branding.ErrPaletteNotFound):
			// keep style = DefaultThemeStyle; cache the negative result
		default:
			// Transient error — surface the default this request,
			// don't cache (so the next request retries the store).
			tm.observe(ThemeCacheResultError)
			return branding.DefaultThemeStyle
		}
	}

	tm.cache.put(tenant.ID, style)
	tm.observe(ThemeCacheResultMiss)
	return style
}

func (tm *ThemeMiddleware) observe(result string) {
	if tm.metrics == nil {
		return
	}
	tm.metrics.ObserveThemeCacheLookup(result)
}

// themeCache is the per-middleware in-memory store. A single mutex is
// sufficient: the critical section is a single map operation and the
// guarded value is a small struct (template.CSS string header +
// time.Time). If contention ever shows up in profiles, swap for
// sync.Map without changing the public surface.
type themeCache struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[uuid.UUID]themeCacheEntry
}

type themeCacheEntry struct {
	style     template.CSS
	expiresAt time.Time
}

func (c *themeCache) get(id uuid.UUID) (template.CSS, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[id]
	if !ok || !c.now().Before(entry.expiresAt) {
		return "", false
	}
	return entry.style, true
}

func (c *themeCache) put(id uuid.UUID, style template.CSS) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[id] = themeCacheEntry{
		style:     style,
		expiresAt: c.now().Add(c.ttl),
	}
}

func (c *themeCache) invalidate(id uuid.UUID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[id]; !ok {
		return false
	}
	delete(c.entries, id)
	return true
}
