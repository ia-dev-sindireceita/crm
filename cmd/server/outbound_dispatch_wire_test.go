package main

import (
	"context"
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

func TestBuildWhatsAppOutboundEntry_DisabledWhenTokenMissing(t *testing.T) {
	t.Parallel()
	// No META_GRAPH_TOKEN → ok=false, nil entry. pool/rdb/flag are unused
	// on this path.
	entry, ok := buildWhatsAppOutboundEntry(func(string) string { return "" }, nil, nil, nil)
	if ok {
		t.Fatalf("ok = true, want false when no graph token is set")
	}
	if entry != nil {
		t.Fatalf("entry = %v, want nil when disabled", entry)
	}
}

func TestAssembleWhatsAppOutboundEntry_RoutesThroughStack(t *testing.T) {
	t.Parallel()
	sender := &dispatchStubSender{wamid: "wamid.assembled"}
	oc := assembleWhatsAppOutboundEntry(sender, nil /* no limiter */, 600)
	tenant := uuid.New()

	// The entry always calls through to the sender — channel-based
	// routing is the combined Router's job (inbox_wire_real.go), not
	// this decorated entry's.
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
}

func TestAssembleWhatsAppOutboundEntry_ReturnsOutboundChannel(t *testing.T) {
	t.Parallel()
	// Compile-time-ish guard: the assembled value satisfies the port the
	// send-outbound use case consumes.
	var _ inbox.OutboundChannel = assembleWhatsAppOutboundEntry(&dispatchStubSender{}, nil, 600)
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
