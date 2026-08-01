package main

// Composition-root tests for buildMessengerOutboundEntry, mirroring
// outbound_dispatch_wire_test.go's WhatsApp coverage.
//
// Deliberately does NOT test the token-present ("ok=true") path here: that
// would call the real channelmessenger.New against prometheus.DefaultRegisterer,
// and a second call anywhere else in this test binary (e.g. an equivalent
// WhatsApp test) would panic with "duplicate metrics collector registration
// attempted" — the exact bug class fixed earlier for WhatsApp. The sender's
// own behavior (including its metrics) is already covered at the package
// level by internal/adapter/channel/messenger/sender_test.go against a
// fresh, isolated registry per test.

import (
	"testing"
)

func TestBuildMessengerOutboundEntry_DisabledWhenTokenMissing(t *testing.T) {
	t.Parallel()
	entry, ok := buildMessengerOutboundEntry(func(string) string { return "" }, nil, nil, nil)
	if ok {
		t.Fatalf("ok = true, want false when no graph token is set")
	}
	if entry != nil {
		t.Fatalf("entry = %v, want nil when disabled", entry)
	}
}

func TestMessengerOutboundGraphToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "both unset", env: map[string]string{}, want: ""},
		{
			name: "falls back to shared META_GRAPH_TOKEN",
			env:  map[string]string{"META_GRAPH_TOKEN": "whatsapp-and-messenger-token"},
			want: "whatsapp-and-messenger-token",
		},
		{
			name: "dedicated override wins over the shared token",
			env: map[string]string{
				"META_GRAPH_TOKEN":           "shared-token",
				"META_MESSENGER_GRAPH_TOKEN": "messenger-only-token",
			},
			want: "messenger-only-token",
		},
		{
			name: "dedicated token alone (no shared token configured)",
			env:  map[string]string{"META_MESSENGER_GRAPH_TOKEN": "messenger-only-token"},
			want: "messenger-only-token",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(k string) string { return tc.env[k] }
			if got := messengerOutboundGraphToken(getenv); got != tc.want {
				t.Fatalf("messengerOutboundGraphToken = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMessengerOutboundRateMaxPerMin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		want int
	}{
		{name: "unset falls back to default", env: map[string]string{}, want: defaultMessengerRateMaxPerMin},
		{name: "invalid falls back to default", env: map[string]string{"MESSENGER_RATE_MAX_PER_MIN": "abc"}, want: defaultMessengerRateMaxPerMin},
		{name: "zero falls back to default", env: map[string]string{"MESSENGER_RATE_MAX_PER_MIN": "0"}, want: defaultMessengerRateMaxPerMin},
		{name: "valid override", env: map[string]string{"MESSENGER_RATE_MAX_PER_MIN": "42"}, want: 42},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(k string) string { return tc.env[k] }
			if got := messengerOutboundRateMaxPerMin(getenv); got != tc.want {
				t.Fatalf("messengerOutboundRateMaxPerMin = %d, want %d", got, tc.want)
			}
		})
	}
}
