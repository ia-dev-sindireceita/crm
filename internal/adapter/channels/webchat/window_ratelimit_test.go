package webchat

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestWindowRateLimiter_SessionBucket_LimitAndReset(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rl := newWindowRateLimiterWithClock(clk.now)
	ctx := context.Background()
	key := "wc.sess.tenant.iphash"

	// D5: 10 session-creates / minute. The 11th is denied.
	for i := 0; i < 10; i++ {
		ok, _, err := rl.Allow(ctx, key)
		if err != nil || !ok {
			t.Fatalf("call %d: Allow = (%v, %v), want allowed", i+1, ok, err)
		}
	}
	ok, retry, _ := rl.Allow(ctx, key)
	if ok {
		t.Fatalf("11th call allowed, want denied")
	}
	if retry <= 0 || retry > time.Minute {
		t.Fatalf("retryAfter = %v, want (0, 1m]", retry)
	}

	// After the window elapses the bucket refills.
	clk.advance(time.Minute)
	if ok, _, _ := rl.Allow(ctx, key); !ok {
		t.Fatalf("after window reset, Allow denied; want allowed")
	}
}

func TestWindowRateLimiter_MessageBucket_HigherLimit(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rl := newWindowRateLimiterWithClock(clk.now)
	ctx := context.Background()
	key := "wc.msg.session-id"

	// D5: 60 messages / minute.
	for i := 0; i < 60; i++ {
		if ok, _, _ := rl.Allow(ctx, key); !ok {
			t.Fatalf("message %d denied, want allowed", i+1)
		}
	}
	if ok, _, _ := rl.Allow(ctx, key); ok {
		t.Fatalf("61st message allowed, want denied")
	}
}

func TestWindowRateLimiter_KeysAreIndependent(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rl := newWindowRateLimiterWithClock(clk.now)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		rl.Allow(ctx, "wc.sess.a")
	}
	// A different session key has its own fresh bucket.
	if ok, _, _ := rl.Allow(ctx, "wc.sess.b"); !ok {
		t.Fatalf("independent key denied, want allowed")
	}
}

func TestWindowRateLimiter_UnknownPrefixUsesFallback(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rl := newWindowRateLimiterWithClock(clk.now)
	ctx := context.Background()
	// Fallback is 60/min.
	for i := 0; i < 60; i++ {
		if ok, _, _ := rl.Allow(ctx, "other.key"); !ok {
			t.Fatalf("fallback call %d denied, want allowed", i+1)
		}
	}
	if ok, _, _ := rl.Allow(ctx, "other.key"); ok {
		t.Fatalf("61st fallback call allowed, want denied")
	}
}

func TestNewWindowRateLimiter_Production(t *testing.T) {
	rl := NewWindowRateLimiter()
	if ok, _, err := rl.Allow(context.Background(), "wc.sess.x"); err != nil || !ok {
		t.Fatalf("production limiter first call = (%v, %v), want allowed", ok, err)
	}
}
