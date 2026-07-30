package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

func TestRateLimited_Allowed_Delegates(t *testing.T) {
	t.Parallel()
	inner := newRecordingChannel("wamid.1")
	lim := &fakeLimiter{allow: true}
	d := NewRateLimited(inner, lim, time.Minute, 600, nil)

	got, err := d.SendMessage(context.Background(), newOutboundMessage(uuid.New(), "whatsapp", ""))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got != "wamid.1" {
		t.Errorf("wamid = %q, want wamid.1", got)
	}
	if inner.callCount() != 1 {
		t.Errorf("inner calls = %d, want 1", inner.callCount())
	}
	if lim.callCount() != 1 {
		t.Errorf("limiter calls = %d, want 1", lim.callCount())
	}
}

func TestRateLimited_OverLimit_TransientNoSend(t *testing.T) {
	t.Parallel()
	inner := newRecordingChannel("wamid.1")
	lim := &fakeLimiter{allow: false, retryAfter: 3 * time.Second}
	d := NewRateLimited(inner, lim, time.Minute, 600, nil)

	_, err := d.SendMessage(context.Background(), newOutboundMessage(uuid.New(), "whatsapp", ""))
	if !errors.Is(err, inbox.ErrChannelTransient) {
		t.Fatalf("err = %v, want ErrChannelTransient", err)
	}
	if inner.callCount() != 0 {
		t.Errorf("inner calls = %d, want 0 (over-limit must not send)", inner.callCount())
	}
}

func TestRateLimited_LimiterError_FailsClosed(t *testing.T) {
	t.Parallel()
	inner := newRecordingChannel("wamid.1")
	lim := &fakeLimiter{allow: true, err: errors.New("redis down")}
	d := NewRateLimited(inner, lim, time.Minute, 600, nil)

	_, err := d.SendMessage(context.Background(), newOutboundMessage(uuid.New(), "whatsapp", ""))
	if !errors.Is(err, inbox.ErrChannelTransient) {
		t.Fatalf("err = %v, want ErrChannelTransient", err)
	}
	if inner.callCount() != 0 {
		t.Errorf("inner calls = %d, want 0 (limiter error must fail closed)", inner.callCount())
	}
}

func TestRateLimited_DisabledConfig_ReturnsInner(t *testing.T) {
	t.Parallel()
	inner := newRecordingChannel("wamid.1")
	cases := []struct {
		name    string
		limiter RateLimiter
		window  time.Duration
		max     int
	}{
		{"nil limiter", nil, time.Minute, 600},
		{"zero max", &fakeLimiter{allow: false}, time.Minute, 0},
		{"zero window", &fakeLimiter{allow: false}, 0, 600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewRateLimited(inner, tc.limiter, tc.window, tc.max, nil)
			// A disabled decorator returns inner unchanged, so a send goes
			// straight through with no limiter consulted.
			if _, err := d.SendMessage(context.Background(), newOutboundMessage(uuid.New(), "whatsapp", "")); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
		})
	}
	if inner.callCount() != len(cases) {
		t.Errorf("inner calls = %d, want %d", inner.callCount(), len(cases))
	}
}

func TestRateLimited_DefaultKeyFunc_BucketsPerTenantChannel(t *testing.T) {
	t.Parallel()
	inner := newRecordingChannel("wamid.1")
	lim := &fakeLimiter{allow: true}
	d := NewRateLimited(inner, lim, time.Minute, 600, nil)
	tenant := uuid.New()
	if _, err := d.SendMessage(context.Background(), newOutboundMessage(tenant, "whatsapp", "")); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	want := "out:whatsapp:" + tenant.String()
	if lim.keys[0] != want {
		t.Errorf("key = %q, want %q", lim.keys[0], want)
	}
}

func TestRateLimited_CustomKeyFunc(t *testing.T) {
	t.Parallel()
	inner := newRecordingChannel("wamid.1")
	lim := &fakeLimiter{allow: true}
	d := NewRateLimited(inner, lim, time.Minute, 600, func(m inbox.OutboundMessage) string {
		return "conv:" + m.ConversationID.String()
	})
	m := newOutboundMessage(uuid.New(), "whatsapp", "")
	if _, err := d.SendMessage(context.Background(), m); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if lim.keys[0] != "conv:"+m.ConversationID.String() {
		t.Errorf("key = %q, want conv-scoped", lim.keys[0])
	}
}
