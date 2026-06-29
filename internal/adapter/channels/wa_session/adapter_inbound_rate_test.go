package wa_session

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// inboundMsg builds a minimally-valid inbound SessionMessage for tenant
// tid — one that clears every border drop so it reaches the per-tenant
// inbound rate gate and (when allowed) HandleInbound.
func inboundMsg(tid uuid.UUID, id string) SessionMessage {
	return SessionMessage{
		TenantID:    tid,
		MessageID:   id,
		SenderPhone: "5511999990001",
		Body:        "oi",
	}
}

// TestReceive_InboundRateLimit is the SIN-66262 F1 regression table: it
// proves Receive caps inbound volume per tenant. Events within the cap
// are delivered; the first event over the cap is rejected with
// ErrInboundRateLimited and never reaches HandleInbound, so the
// per-tenant DB write amplification is bounded. Uses the real
// InMemoryRateLimiter so the window/counter is exercised end to end.
func TestReceive_InboundRateLimit(t *testing.T) {
	const capPerWin = 3
	in := &fakeInbound{}
	a, err := New(in, &fakeSender{}, enabledFlag(), NewInMemoryRateLimiter(),
		WithConfig(Config{InboundRateMaxPerMin: capPerWin}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The first capPerWin distinct messages are accepted and persisted.
	for i := 0; i < capPerWin; i++ {
		if err := a.Receive(context.Background(), inboundMsg(testTenant, "ok-"+string(rune('a'+i)))); err != nil {
			t.Fatalf("hit %d: Receive = %v, want nil", i, err)
		}
	}
	if in.calls != capPerWin {
		t.Fatalf("HandleInbound calls = %d, want %d", in.calls, capPerWin)
	}

	// The next message, over the cap, is rejected and NOT persisted.
	if err := a.Receive(context.Background(), inboundMsg(testTenant, "over")); !errors.Is(err, ErrInboundRateLimited) {
		t.Fatalf("over-cap Receive = %v, want ErrInboundRateLimited", err)
	}
	if in.calls != capPerWin {
		t.Errorf("HandleInbound calls after over-cap = %d, want %d (no DB write past the cap)", in.calls, capPerWin)
	}
}

// TestReceive_InboundRateLimit_PerTenant proves the cap is keyed per
// tenant: exhausting tenant A's budget does not throttle tenant B.
func TestReceive_InboundRateLimit_PerTenant(t *testing.T) {
	const capPerWin = 2
	in := &fakeInbound{}
	a, err := New(in, &fakeSender{}, enabledFlag(), NewInMemoryRateLimiter(),
		WithConfig(Config{InboundRateMaxPerMin: capPerWin}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tenantB := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	// Exhaust tenant A's budget.
	for i := 0; i < capPerWin; i++ {
		if err := a.Receive(context.Background(), inboundMsg(testTenant, "a"+string(rune('0'+i)))); err != nil {
			t.Fatalf("tenant A hit %d: Receive = %v", i, err)
		}
	}
	if err := a.Receive(context.Background(), inboundMsg(testTenant, "a-over")); !errors.Is(err, ErrInboundRateLimited) {
		t.Fatalf("tenant A over cap = %v, want ErrInboundRateLimited", err)
	}

	// Tenant B still has a full, independent budget.
	if err := a.Receive(context.Background(), inboundMsg(tenantB, "b0")); err != nil {
		t.Fatalf("tenant B within cap = %v, want nil", err)
	}
}

// TestReceive_InboundRateKeyAndError pins the limiter key namespace
// (wa_session:in:<tenant>, distinct from the outbound wa_session:out:
// key so the two directions throttle independently) and that a limiter
// error propagates from Receive without persisting.
func TestReceive_InboundRateKeyAndError(t *testing.T) {
	t.Run("key is per-tenant inbound", func(t *testing.T) {
		rate := allowAllRate()
		in := &fakeInbound{}
		a := mustAdapter(t, in, &fakeSender{}, enabledFlag(), rate)
		if err := a.Receive(context.Background(), inboundMsg(testTenant, "m")); err != nil {
			t.Fatalf("Receive: %v", err)
		}
		want := "wa_session:in:" + testTenant.String()
		if rate.lastKey != want {
			t.Errorf("rate key = %q, want %q", rate.lastKey, want)
		}
	})

	t.Run("limiter error propagates, no persist", func(t *testing.T) {
		in := &fakeInbound{}
		a := mustAdapter(t, in, &fakeSender{}, enabledFlag(), &fakeRate{err: errBoom})
		err := a.Receive(context.Background(), inboundMsg(testTenant, "m"))
		if !errors.Is(err, errBoom) {
			t.Fatalf("Receive err = %v, want errBoom", err)
		}
		if in.calls != 0 {
			t.Errorf("HandleInbound calls = %d, want 0 on limiter error", in.calls)
		}
	})
}

// TestConfigFromEnv_InboundRate covers the
// WA_SESSION_INBOUND_RATE_MAX_PER_MIN override and its safe default.
func TestConfigFromEnv_InboundRate(t *testing.T) {
	if got := DefaultConfig().InboundRateMaxPerMin; got != defaultInboundRateMaxPerMinute {
		t.Fatalf("default InboundRateMaxPerMin = %d, want %d", got, defaultInboundRateMaxPerMinute)
	}

	env := map[string]string{EnvSessionInboundRateMax: "250"}
	if got := ConfigFromEnv(func(k string) string { return env[k] }).InboundRateMaxPerMin; got != 250 {
		t.Errorf("InboundRateMaxPerMin override = %d, want 250", got)
	}

	// An invalid value falls back to the default rather than silently
	// disabling the cap (parsePositiveInt rejects 0/negatives/garbage).
	bad := map[string]string{EnvSessionInboundRateMax: "0"}
	if got := ConfigFromEnv(func(k string) string { return bad[k] }).InboundRateMaxPerMin; got != defaultInboundRateMaxPerMinute {
		t.Errorf("InboundRateMaxPerMin on invalid = %d, want default %d", got, defaultInboundRateMaxPerMinute)
	}
}
