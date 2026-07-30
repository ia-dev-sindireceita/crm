package main

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channel/dispatch"
	"github.com/pericles-luz/crm/internal/inbox"
)

// dispatchStubSender is a fake carrier sender for the wiring assembly test.
type dispatchStubSender struct {
	wamid string
	calls int
}

func (r *dispatchStubSender) SendMessage(_ context.Context, _ inbox.OutboundMessage) (string, error) {
	r.calls++
	return r.wamid, nil
}

func TestBuildWhatsAppOutbound_DisabledWhenTokenMissing(t *testing.T) {
	t.Parallel()
	// No META_GRAPH_TOKEN → graceful no-op router (never nil, never panics,
	// zero outbound HTTP). pool/rdb/flag are unused on this path.
	oc := buildWhatsAppOutbound(func(string) string { return "" }, nil, nil, nil)
	if oc == nil {
		t.Fatal("buildWhatsAppOutbound returned nil; want no-op router")
	}
	_, err := oc.SendMessage(context.Background(), inbox.OutboundMessage{Channel: "whatsapp", TenantID: uuid.New()})
	if !errors.Is(err, inbox.ErrChannelDisabled) {
		t.Errorf("no-op router err = %v, want ErrChannelDisabled", err)
	}
}

func TestAssembleWhatsAppOutbound_RoutesWhatsAppThroughStack(t *testing.T) {
	t.Parallel()
	sender := &dispatchStubSender{wamid: "wamid.assembled"}
	oc := assembleWhatsAppOutbound(sender, nil /* no limiter */, 600)
	tenant := uuid.New()

	// whatsapp is routed through to the sender.
	got, err := oc.SendMessage(context.Background(), inbox.OutboundMessage{
		Channel:        "whatsapp",
		TenantID:       tenant,
		ConversationID: uuid.New(),
		ToExternalID:   "+5511999990001",
		Body:           "oi",
		IdempotencyKey: "k1",
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got != "wamid.assembled" {
		t.Errorf("wamid = %q, want wamid.assembled", got)
	}

	// Same idempotency key → deduped, no second carrier call.
	if _, err := oc.SendMessage(context.Background(), inbox.OutboundMessage{
		Channel: "whatsapp", TenantID: tenant, ConversationID: uuid.New(),
		ToExternalID: "+5511999990001", Body: "oi", IdempotencyKey: "k1",
	}); err != nil {
		t.Fatalf("dedup send: %v", err)
	}
	if sender.calls != 1 {
		t.Errorf("carrier calls = %d, want 1 (idempotent stack)", sender.calls)
	}

	// A non-whatsapp channel is not routed by this dispatcher.
	if _, err := oc.SendMessage(context.Background(), inbox.OutboundMessage{Channel: "sms", TenantID: tenant}); !errors.Is(err, inbox.ErrChannelDisabled) {
		t.Errorf("unrouted channel err = %v, want ErrChannelDisabled", err)
	}
}

func TestAssembleWhatsAppOutbound_ReturnsOutboundChannel(t *testing.T) {
	t.Parallel()
	// Compile-time-ish guard: the assembled value satisfies the port the
	// send-outbound use case consumes.
	var _ inbox.OutboundChannel = assembleWhatsAppOutbound(&dispatchStubSender{}, nil, 600)
	var _ dispatch.Ledger = dispatch.NewMemoryLedger()
}

func TestOutboundRateMaxPerMin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		val  string
		want int
	}{
		{"unset", "", defaultOutboundRateMaxPerMin},
		{"valid", "120", 120},
		{"zero", "0", defaultOutboundRateMaxPerMin},
		{"negative", "-5", defaultOutboundRateMaxPerMin},
		{"garbage", "abc", defaultOutboundRateMaxPerMin},
		{"padded", "  90  ", 90},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := outboundRateMaxPerMin(func(string) string { return tc.val })
			if got != tc.want {
				t.Errorf("outboundRateMaxPerMin(%q) = %d, want %d", tc.val, got, tc.want)
			}
		})
	}
}
