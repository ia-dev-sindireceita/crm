package tenancy_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/tenancy"
)

// stubResolver counts upstream calls so we can assert cache hits/misses.
type stubResolver struct {
	calls   atomic.Int64
	tenant  *tenancy.Tenant
	err     error
	respond func(host string) (*tenancy.Tenant, error)
}

func (s *stubResolver) ResolveByHost(_ context.Context, host string) (*tenancy.Tenant, error) {
	s.calls.Add(1)
	if s.respond != nil {
		return s.respond(host)
	}
	return s.tenant, s.err
}

func TestCachingResolver_HitsThenServesFromCache(t *testing.T) {
	t.Parallel()

	want := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	upstream := &stubResolver{tenant: want}
	cache := tenancy.NewCachingResolver(upstream, 5*time.Minute)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		got, err := cache.ResolveByHost(ctx, "acme.crm.local")
		if err != nil {
			t.Fatalf("call %d err: %v", i, err)
		}
		if got != want {
			t.Fatalf("call %d returned %#v, want %#v", i, got, want)
		}
	}
	if got := upstream.calls.Load(); got != 1 {
		t.Fatalf("upstream called %d times, want exactly 1 (cache miss + 2 hits)", got)
	}
}

func TestCachingResolver_CachesNotFound(t *testing.T) {
	t.Parallel()

	upstream := &stubResolver{err: tenancy.ErrTenantNotFound}
	cache := tenancy.NewCachingResolver(upstream, time.Minute)

	for i := 0; i < 4; i++ {
		_, err := cache.ResolveByHost(context.Background(), "ghost.crm.local")
		if !errors.Is(err, tenancy.ErrTenantNotFound) {
			t.Fatalf("call %d err = %v, want ErrTenantNotFound", i, err)
		}
	}
	if got := upstream.calls.Load(); got != 1 {
		t.Fatalf("upstream called %d times, want 1 (negative result cached)", got)
	}
}

func TestCachingResolver_DoesNotCacheTransientErrors(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection reset")
	upstream := &stubResolver{err: boom}
	cache := tenancy.NewCachingResolver(upstream, time.Minute)

	for i := 0; i < 3; i++ {
		if _, err := cache.ResolveByHost(context.Background(), "acme.crm.local"); !errors.Is(err, boom) {
			t.Fatalf("call %d err = %v, want %v", i, err, boom)
		}
	}
	if got := upstream.calls.Load(); got != 3 {
		t.Fatalf("upstream called %d times, want 3 (transient errors must not poison cache)", got)
	}
}

func TestCachingResolver_TTLExpiry(t *testing.T) {
	t.Parallel()

	upstream := &stubResolver{tenant: &tenancy.Tenant{ID: uuid.New(), Host: "acme.crm.local"}}
	cache := tenancy.NewCachingResolver(upstream, time.Minute)

	// Override the clock for deterministic expiry.
	now := time.Now()
	tenancy.SetClockForTest(cache, func() time.Time { return now })

	if _, err := cache.ResolveByHost(context.Background(), "acme.crm.local"); err != nil {
		t.Fatalf("initial call: %v", err)
	}
	// Within TTL → cache hit.
	if _, err := cache.ResolveByHost(context.Background(), "acme.crm.local"); err != nil {
		t.Fatalf("within ttl: %v", err)
	}
	// Advance past TTL → cache miss.
	now = now.Add(2 * time.Minute)
	if _, err := cache.ResolveByHost(context.Background(), "acme.crm.local"); err != nil {
		t.Fatalf("after ttl: %v", err)
	}

	if got := upstream.calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (initial + post-expiry)", got)
	}
}

func TestCachingResolver_Invalidate(t *testing.T) {
	t.Parallel()

	upstream := &stubResolver{tenant: &tenancy.Tenant{ID: uuid.New(), Host: "acme.crm.local"}}
	cache := tenancy.NewCachingResolver(upstream, time.Hour)

	if _, err := cache.ResolveByHost(context.Background(), "acme.crm.local"); err != nil {
		t.Fatal(err)
	}
	cache.Invalidate("acme.crm.local")
	if _, err := cache.ResolveByHost(context.Background(), "acme.crm.local"); err != nil {
		t.Fatal(err)
	}
	if got := upstream.calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 after Invalidate", got)
	}
}

func TestCachingResolver_DefaultTTL(t *testing.T) {
	t.Parallel()

	upstream := &stubResolver{tenant: &tenancy.Tenant{ID: uuid.New()}}
	cache := tenancy.NewCachingResolver(upstream, 0) // 0 → DefaultCacheTTL

	for i := 0; i < 5; i++ {
		if _, err := cache.ResolveByHost(context.Background(), "acme.crm.local"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := upstream.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (default TTL should keep entries warm)", got)
	}
}

func TestCachingResolver_NilUpstreamErrors(t *testing.T) {
	t.Parallel()

	cache := &tenancy.CachingResolver{}
	if _, err := cache.ResolveByHost(context.Background(), "x"); err == nil {
		t.Fatal("expected error from nil upstream, got nil")
	}
}
