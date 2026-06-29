package wasession

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestQRCache_PutGet_RoundTrip(t *testing.T) {
	t.Parallel()
	c := NewQRCache()
	tenant := uuid.New()
	qr := QRCode{Code: NewCredential("pair-code-abc"), ExpiresAt: time.Now().Add(time.Minute)}

	c.Put(tenant, qr)
	got, ok := c.Get(tenant)
	if !ok {
		t.Fatal("Get ok = false, want true after Put")
	}
	if got.Code.Reveal() != "pair-code-abc" {
		t.Fatalf("Code = %q, want pair-code-abc", got.Code.Reveal())
	}
}

func TestQRCache_Get_MissingTenant(t *testing.T) {
	t.Parallel()
	c := NewQRCache()
	if _, ok := c.Get(uuid.New()); ok {
		t.Fatal("Get on empty cache ok = true, want false")
	}
}

func TestQRCache_Put_IgnoresZeroCredential(t *testing.T) {
	t.Parallel()
	c := NewQRCache()
	tenant := uuid.New()
	live := QRCode{Code: NewCredential("live"), ExpiresAt: time.Now().Add(time.Minute)}
	c.Put(tenant, live)

	// A zero-credential emit must not wipe the live code.
	c.Put(tenant, QRCode{})
	got, ok := c.Get(tenant)
	if !ok || got.Code.Reveal() != "live" {
		t.Fatalf("after zero Put: ok=%v code=%q, want ok=true code=live", ok, got.Code.Reveal())
	}
}

func TestQRCache_Get_ExpiredIsDropped(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	c := NewQRCache(WithQRClock(func() time.Time { return now }))
	tenant := uuid.New()
	c.Put(tenant, QRCode{Code: NewCredential("old"), ExpiresAt: now.Add(-time.Second)})

	if _, ok := c.Get(tenant); ok {
		t.Fatal("Get on expired entry ok = true, want false")
	}
	// Confirm the expired entry was evicted (no lingering state).
	c.mu.RLock()
	_, present := c.m[tenant]
	c.mu.RUnlock()
	if present {
		t.Fatal("expired entry still present after Get, want evicted")
	}
}

func TestQRCache_Get_ZeroExpiryNeverExpires(t *testing.T) {
	t.Parallel()
	far := time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewQRCache(WithQRClock(func() time.Time { return far }))
	tenant := uuid.New()
	c.Put(tenant, QRCode{Code: NewCredential("eternal")}) // zero ExpiresAt

	if _, ok := c.Get(tenant); !ok {
		t.Fatal("zero-ExpiresAt entry expired, want non-expiring")
	}
}

func TestQRCache_Clear(t *testing.T) {
	t.Parallel()
	c := NewQRCache()
	tenant := uuid.New()
	c.Put(tenant, QRCode{Code: NewCredential("x"), ExpiresAt: time.Now().Add(time.Minute)})
	c.Clear(tenant)
	if _, ok := c.Get(tenant); ok {
		t.Fatal("Get after Clear ok = true, want false")
	}
	// Clearing an absent tenant must be a no-op (no panic).
	c.Clear(uuid.New())
}

func TestQRCache_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	c := NewQRCache()
	tenant := uuid.New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.Put(tenant, QRCode{Code: NewCredential("c"), ExpiresAt: time.Now().Add(time.Minute)})
		}()
		go func() { defer wg.Done(); _, _ = c.Get(tenant) }()
	}
	wg.Wait()
}
