package middleware_test

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// fakeStore is a deterministic branding.PaletteStore for testing. The
// behaviour is driven by per-tenant callbacks so each test can pin the
// exact error/value combination it cares about.
type fakeStore struct {
	mu    sync.Mutex
	calls int32
	by    map[uuid.UUID]storeAnswer
}

type storeAnswer struct {
	palette branding.Palette
	err     error
}

func (s *fakeStore) GetByTenantID(_ context.Context, id uuid.UUID) (branding.Palette, error) {
	atomic.AddInt32(&s.calls, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.by[id]
	if !ok {
		return branding.Palette{}, branding.ErrPaletteNotFound
	}
	return a.palette, a.err
}

func (s *fakeStore) Calls() int { return int(atomic.LoadInt32(&s.calls)) }

// recorderMetrics is a middleware.ThemeMetrics that just tallies
// per-label observations. Tests assert on the counts.
type recorderMetrics struct {
	mu     sync.Mutex
	counts map[string]int
}

func newRecorder() *recorderMetrics { return &recorderMetrics{counts: map[string]int{}} }

func (r *recorderMetrics) ObserveThemeCacheLookup(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[result]++
}

func (r *recorderMetrics) snapshot() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.counts))
	for k, v := range r.counts {
		out[k] = v
	}
	return out
}

// fakeClock is a monotonic, manually-advanced clock so cache TTL tests
// don't rely on real-time sleeps.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// captureStyle is the http.Handler the middleware wraps in tests. It
// records the style value attached to the context so assertions can
// pin exact bytes without going through a template render.
type captureStyle struct {
	mu  sync.Mutex
	got template.CSS
}

func (c *captureStyle) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = branding.ThemeStyleFromContext(r.Context())
}

func (c *captureStyle) Style() template.CSS {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got
}

func themeRequestWithTenant(tenantID uuid.UUID) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/hello", nil)
	tenant := &tenancy.Tenant{ID: tenantID, Name: "acme", Host: "acme.example.test"}
	ctx := tenancy.WithContext(r.Context(), tenant)
	return r.WithContext(ctx)
}

func TestTheme_MissPopulatesCacheAndIncrementsMetric(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	tid := uuid.New()
	custom := branding.Palette{
		Primary:       branding.RGB{R: 0xde, G: 0xad, B: 0xbe},
		Secondary:     branding.RGB{R: 0xef, G: 0x12, B: 0x34},
		Accent:        branding.RGB{R: 0x55, G: 0x66, B: 0x77},
		Foreground:    branding.RGB{R: 0x00, G: 0x00, B: 0x00},
		Background:    branding.RGB{R: 0xff, G: 0xff, B: 0xff},
		TextOnPrimary: branding.RGB{R: 0xff, G: 0xff, B: 0xff},
	}
	store := &fakeStore{by: map[uuid.UUID]storeAnswer{tid: {palette: custom}}}
	rec := newRecorder()
	tm := middleware.NewTheme(middleware.ThemeConfig{
		Store:   store,
		TTL:     60 * time.Second,
		Now:     clock.Now,
		Metrics: rec,
	})
	cap := &captureStyle{}
	h := tm.Handler(cap)

	h.ServeHTTP(httptest.NewRecorder(), themeRequestWithTenant(tid))
	want := branding.ThemeStyleFromPalette(custom)
	if got := cap.Style(); got != want {
		t.Fatalf("first request: got %q, want %q", got, want)
	}
	if calls := store.Calls(); calls != 1 {
		t.Fatalf("expected 1 store call, got %d", calls)
	}
	if rec.snapshot()[middleware.ThemeCacheResultMiss] != 1 {
		t.Fatalf("miss not recorded: %+v", rec.snapshot())
	}

	// Same tenant within TTL → cache hit, no second store call.
	h.ServeHTTP(httptest.NewRecorder(), themeRequestWithTenant(tid))
	if calls := store.Calls(); calls != 1 {
		t.Fatalf("expected cache hit (still 1 call), got %d", calls)
	}
	snap := rec.snapshot()
	if snap[middleware.ThemeCacheResultHit] != 1 || snap[middleware.ThemeCacheResultMiss] != 1 {
		t.Fatalf("hit/miss accounting: %+v", snap)
	}
}

func TestTheme_NotFoundCachesDefault(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	tid := uuid.New()
	store := &fakeStore{by: map[uuid.UUID]storeAnswer{}} // every lookup → ErrPaletteNotFound
	rec := newRecorder()
	tm := middleware.NewTheme(middleware.ThemeConfig{
		Store: store, TTL: 60 * time.Second, Now: clock.Now, Metrics: rec,
	})
	cap := &captureStyle{}
	h := tm.Handler(cap)

	h.ServeHTTP(httptest.NewRecorder(), themeRequestWithTenant(tid))
	if got := cap.Style(); got != branding.DefaultThemeStyle {
		t.Fatalf("expected DefaultThemeStyle on not-found, got %q", got)
	}
	// Second request inside TTL: must NOT re-hit the store — negative
	// result is cached so a flood of requests for an unbranded tenant
	// does not hammer the DB.
	h.ServeHTTP(httptest.NewRecorder(), themeRequestWithTenant(tid))
	if calls := store.Calls(); calls != 1 {
		t.Fatalf("not-found should cache: got %d calls", calls)
	}
}

func TestTheme_TransientErrorBypassesCache(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	tid := uuid.New()
	boom := errors.New("db timeout")
	store := &fakeStore{by: map[uuid.UUID]storeAnswer{tid: {err: boom}}}
	rec := newRecorder()
	tm := middleware.NewTheme(middleware.ThemeConfig{
		Store: store, TTL: 60 * time.Second, Now: clock.Now, Metrics: rec,
	})
	cap := &captureStyle{}
	h := tm.Handler(cap)

	h.ServeHTTP(httptest.NewRecorder(), themeRequestWithTenant(tid))
	if got := cap.Style(); got != branding.DefaultThemeStyle {
		t.Fatalf("transient error must surface default, got %q", got)
	}
	h.ServeHTTP(httptest.NewRecorder(), themeRequestWithTenant(tid))
	if calls := store.Calls(); calls != 2 {
		t.Fatalf("transient error must NOT cache; expected 2 calls, got %d", calls)
	}
	if rec.snapshot()[middleware.ThemeCacheResultError] != 2 {
		t.Fatalf("error metric not recorded twice: %+v", rec.snapshot())
	}
}

func TestTheme_TTLExpiry(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	tid := uuid.New()
	store := &fakeStore{by: map[uuid.UUID]storeAnswer{tid: {palette: branding.DefaultPalette}}}
	tm := middleware.NewTheme(middleware.ThemeConfig{
		Store: store, TTL: 60 * time.Second, Now: clock.Now,
	})
	cap := &captureStyle{}
	h := tm.Handler(cap)

	h.ServeHTTP(httptest.NewRecorder(), themeRequestWithTenant(tid)) // miss
	clock.Advance(59 * time.Second)
	h.ServeHTTP(httptest.NewRecorder(), themeRequestWithTenant(tid)) // hit (within TTL)
	if calls := store.Calls(); calls != 1 {
		t.Fatalf("within TTL: expected 1 store call, got %d", calls)
	}
	clock.Advance(2 * time.Second) // total 61s — past TTL
	h.ServeHTTP(httptest.NewRecorder(), themeRequestWithTenant(tid)) // miss again
	if calls := store.Calls(); calls != 2 {
		t.Fatalf("post-TTL: expected 2 store calls, got %d", calls)
	}
}

func TestTheme_Invalidate(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	tid := uuid.New()
	store := &fakeStore{by: map[uuid.UUID]storeAnswer{tid: {palette: branding.DefaultPalette}}}
	tm := middleware.NewTheme(middleware.ThemeConfig{
		Store: store, TTL: 60 * time.Second, Now: clock.Now,
	})
	cap := &captureStyle{}
	h := tm.Handler(cap)

	h.ServeHTTP(httptest.NewRecorder(), themeRequestWithTenant(tid))
	if !tm.Invalidate(tid) {
		t.Fatal("Invalidate should return true for a present entry")
	}
	if tm.Invalidate(tid) {
		t.Fatal("second Invalidate should return false (entry already evicted)")
	}
	h.ServeHTTP(httptest.NewRecorder(), themeRequestWithTenant(tid))
	if calls := store.Calls(); calls != 2 {
		t.Fatalf("post-invalidate: expected store re-hit, got %d calls", calls)
	}
}

func TestTheme_InvalidateOnNilReceiverIsNoop(t *testing.T) {
	t.Parallel()
	var tm *middleware.ThemeMiddleware
	if tm.Invalidate(uuid.New()) {
		t.Fatal("nil receiver Invalidate must return false")
	}
}

func TestTheme_NoTenantInContextUsesDefault(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	tm := middleware.NewTheme(middleware.ThemeConfig{Metrics: rec})
	cap := &captureStyle{}
	h := tm.Handler(cap)

	// Plain request — no tenancy.WithContext.
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got := cap.Style(); got != branding.DefaultThemeStyle {
		t.Fatalf("no-tenant must use default, got %q", got)
	}
	if rec.snapshot()[middleware.ThemeCacheResultNoTenant] != 1 {
		t.Fatalf("no_tenant metric not recorded: %+v", rec.snapshot())
	}
}

// TestTheme_NilStoreServesDefault asserts the middleware degrades to
// always-default when no PaletteStore is wired (the pre-SIN-63075
// deploy posture). The miss path still records "miss" so the dashboard
// can show "0 hits, N misses" until the store lands.
func TestTheme_NilStoreServesDefault(t *testing.T) {
	t.Parallel()
	tid := uuid.New()
	rec := newRecorder()
	tm := middleware.NewTheme(middleware.ThemeConfig{Store: nil, Metrics: rec})
	cap := &captureStyle{}
	h := tm.Handler(cap)

	h.ServeHTTP(httptest.NewRecorder(), themeRequestWithTenant(tid))
	if got := cap.Style(); got != branding.DefaultThemeStyle {
		t.Fatalf("nil-store should serve default, got %q", got)
	}
	// Cached on miss: second request is a hit.
	h.ServeHTTP(httptest.NewRecorder(), themeRequestWithTenant(tid))
	snap := rec.snapshot()
	if snap[middleware.ThemeCacheResultMiss] != 1 || snap[middleware.ThemeCacheResultHit] != 1 {
		t.Fatalf("hit/miss accounting on nil store: %+v", snap)
	}
}

// TestTheme_DefaultTTLApplied checks the zero-TTL convenience: callers
// that pass ThemeConfig{} (a degenerate wire-up) get the 60s spec
// value, not an immediately-stale cache.
func TestTheme_DefaultTTLApplied(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	tid := uuid.New()
	store := &fakeStore{by: map[uuid.UUID]storeAnswer{tid: {palette: branding.DefaultPalette}}}
	tm := middleware.NewTheme(middleware.ThemeConfig{Store: store, Now: clock.Now})
	cap := &captureStyle{}
	h := tm.Handler(cap)

	h.ServeHTTP(httptest.NewRecorder(), themeRequestWithTenant(tid))
	clock.Advance(middleware.DefaultThemeCacheTTL - time.Second)
	h.ServeHTTP(httptest.NewRecorder(), themeRequestWithTenant(tid)) // still in TTL
	if calls := store.Calls(); calls != 1 {
		t.Fatalf("expected 1 call within default TTL window, got %d", calls)
	}
}
