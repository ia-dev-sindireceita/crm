package webchat

import (
	"context"
	"testing"
	"time"
)

// --- Broker connection caps (ADR-0021 D5, threat T5) ---

func TestBroker_PerSessionCap(t *testing.T) {
	b := NewBroker()
	if _, ok := b.Subscribe("s1", ""); !ok {
		t.Fatalf("first subscribe denied, want allowed")
	}
	// maxStreamsPerSession == 1, so a second concurrent stream for the
	// same session must be rejected.
	if _, ok := b.Subscribe("s1", ""); ok {
		t.Fatalf("second subscribe for same session allowed, want denied")
	}
}

func TestBroker_PerTenantIPCap(t *testing.T) {
	b := NewBroker()
	ipKey := "iphash-A"
	subs := make([]chan string, 0, maxStreamsPerTenantIP)
	// Five distinct sessions sharing one (tenant × IP) bucket fill it.
	for i := 0; i < maxStreamsPerTenantIP; i++ {
		ch, ok := b.Subscribe(sessionName(i), ipKey)
		if !ok {
			t.Fatalf("subscribe %d denied, want allowed", i)
		}
		subs = append(subs, ch)
	}
	// The (cap+1)th distinct session on the same IP is rejected even
	// though its own per-session count is zero.
	if _, ok := b.Subscribe("overflow", ipKey); ok {
		t.Fatalf("subscribe past per-IP cap allowed, want denied")
	}
	// A different IP bucket is independent.
	if _, ok := b.Subscribe("other", "iphash-B"); !ok {
		t.Fatalf("subscribe on fresh IP bucket denied, want allowed")
	}
	_ = subs
}

func TestBroker_UnsubscribeReleasesSlots(t *testing.T) {
	b := NewBroker()
	ipKey := "iphash-A"
	var chans []chan string
	for i := 0; i < maxStreamsPerTenantIP; i++ {
		ch, ok := b.Subscribe(sessionName(i), ipKey)
		if !ok {
			t.Fatalf("subscribe %d denied", i)
		}
		chans = append(chans, ch)
	}
	if _, ok := b.Subscribe("overflow", ipKey); ok {
		t.Fatalf("expected IP cap to be full")
	}
	// Releasing one slot must let a new subscriber in (no leak).
	b.Unsubscribe(sessionName(0), ipKey, chans[0])
	ch, ok := b.Subscribe("now-fits", ipKey)
	if !ok {
		t.Fatalf("subscribe after release denied, want allowed")
	}
	// Per-session slot is freed too: re-subscribing the released session
	// succeeds.
	b.Unsubscribe(sessionName(1), ipKey, chans[1])
	if _, ok := b.Subscribe(sessionName(1), ipKey); !ok {
		t.Fatalf("re-subscribe released session denied, want allowed")
	}
	_ = ch
}

func TestBroker_EmptyIPKeyBypassesIPCap(t *testing.T) {
	b := NewBroker()
	// With no IP key, only the per-session cap applies; many distinct
	// sessions can subscribe without tripping the IP cap.
	for i := 0; i < maxStreamsPerTenantIP+3; i++ {
		if _, ok := b.Subscribe(sessionName(i), ""); !ok {
			t.Fatalf("subscribe %d with empty ipKey denied, want allowed", i)
		}
	}
}

func TestBroker_PublishToCappedSubscriber(t *testing.T) {
	b := NewBroker()
	ch, ok := b.Subscribe("s1", "ip")
	if !ok {
		t.Fatalf("subscribe denied")
	}
	b.Publish("s1", "hello")
	select {
	case got := <-ch:
		if got != "hello" {
			t.Fatalf("got %q, want hello", got)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for published payload")
	}
	// Publish to an unknown session is a no-op (must not panic).
	b.Publish("nobody", "x")
}

func sessionName(i int) string {
	return "sess-" + string(rune('a'+i))
}

// --- networkBucket (/24 anti-sybil prefix) ---

func TestNetworkBucket(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ipv4 same /24", "203.0.113.7", "203.0.113.0"},
		{"ipv4 high host same /24", "203.0.113.250", "203.0.113.0"},
		{"ipv4 different /24", "203.0.114.7", "203.0.114.0"},
		{"ipv6 /48", "2001:db8:abcd:1234::1", "2001:db8:abcd::"},
		{"unparseable falls back to self", "not-an-ip", "not-an-ip"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := networkBucket(c.in); got != c.want {
				t.Fatalf("networkBucket(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	// Two hosts in the same /24 must collapse to one bucket.
	if networkBucket("203.0.113.7") != networkBucket("203.0.113.99") {
		t.Fatalf("hosts in same /24 produced different buckets")
	}
}

// --- new WindowRateLimiter rules (D5 /24 + SSE entry) ---

func TestWindowRateLimiter_Sybil24Bucket(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rl := newWindowRateLimiterWithClock(clk.now)
	ctx := context.Background()
	key := "wc.s24.tenant.nethash"
	// D5: 200 session-creates / minute per /24.
	for i := 0; i < 200; i++ {
		if ok, _, _ := rl.Allow(ctx, key); !ok {
			t.Fatalf("/24 call %d denied, want allowed", i+1)
		}
	}
	if ok, _, _ := rl.Allow(ctx, key); ok {
		t.Fatalf("201st /24 call allowed, want denied")
	}
}

func TestWindowRateLimiter_StreamEntryBucket(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rl := newWindowRateLimiterWithClock(clk.now)
	ctx := context.Background()
	key := "wc.stream.session-id"
	// D5: 60 stream connects / minute.
	for i := 0; i < 60; i++ {
		if ok, _, _ := rl.Allow(ctx, key); !ok {
			t.Fatalf("stream connect %d denied, want allowed", i+1)
		}
	}
	if ok, retry, _ := rl.Allow(ctx, key); ok || retry <= 0 {
		t.Fatalf("61st stream connect = (allowed=%v, retry=%v), want denied with retry>0", ok, retry)
	}
	// Distinct from the message bucket — same suffix, different prefix.
	if ok, _, _ := rl.Allow(ctx, "wc.msg.session-id"); !ok {
		t.Fatalf("message bucket shares state with stream bucket")
	}
}
