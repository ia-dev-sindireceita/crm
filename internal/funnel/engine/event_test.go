package engine_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/funnel/engine"
)

func TestDecodeInboundMessage_HappyPath(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	conversationID := uuid.New()
	messageID := uuid.New()
	occurredAt := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	raw, err := engine.EncodeInboundMessage(engine.InboundMessage{
		TenantID:       tenantID,
		ConversationID: conversationID,
		MessageID:      messageID,
		Channel:        "WebChat", // upper-cased on purpose
		Body:           "olá",
		OccurredAt:     occurredAt,
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := engine.DecodeInboundMessage(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.TenantID != tenantID || got.ConversationID != conversationID || got.MessageID != messageID {
		t.Errorf("Decode ids wrong: got %+v", got)
	}
	if got.Channel != "webchat" {
		t.Errorf("channel = %q, want lower-cased %q", got.Channel, "webchat")
	}
	if !got.OccurredAt.Equal(occurredAt) {
		t.Errorf("OccurredAt = %v, want %v", got.OccurredAt, occurredAt)
	}
}

func TestDecodeInboundMessage_RejectsBadPayloads(t *testing.T) {
	t.Parallel()

	tenant := uuid.New()
	conversation := uuid.New()
	message := uuid.New()
	good := engine.InboundMessage{
		TenantID:       tenant,
		ConversationID: conversation,
		MessageID:      message,
		Channel:        "webchat",
		Body:           "olá",
		OccurredAt:     time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
	}
	tests := []struct {
		name   string
		mutate func(*engine.InboundMessageEvent)
	}{
		{name: "nil tenant", mutate: func(e *engine.InboundMessageEvent) { e.TenantID = uuid.Nil.String() }},
		{name: "blank tenant", mutate: func(e *engine.InboundMessageEvent) { e.TenantID = "" }},
		{name: "garbage tenant", mutate: func(e *engine.InboundMessageEvent) { e.TenantID = "not-a-uuid" }},
		{name: "nil conversation", mutate: func(e *engine.InboundMessageEvent) { e.ConversationID = uuid.Nil.String() }},
		{name: "blank message", mutate: func(e *engine.InboundMessageEvent) { e.MessageID = "" }},
		{name: "blank channel", mutate: func(e *engine.InboundMessageEvent) { e.Channel = "" }},
		{name: "zero occurredAt", mutate: func(e *engine.InboundMessageEvent) { e.OccurredAt = time.Time{} }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw, err := engine.EncodeInboundMessage(good)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			// Re-decode into a wire struct, mutate, re-encode.
			ev := engine.InboundMessageEvent{
				TenantID:       good.TenantID.String(),
				ConversationID: good.ConversationID.String(),
				MessageID:      good.MessageID.String(),
				Channel:        good.Channel,
				Body:           good.Body,
				OccurredAt:     good.OccurredAt,
			}
			tt.mutate(&ev)
			bad := mustReencode(t, ev)
			if _, err := engine.DecodeInboundMessage(bad); !errors.Is(err, engine.ErrInvalidEvent) {
				t.Errorf("Decode err = %v, want ErrInvalidEvent (raw=%s)", err, raw)
			}
		})
	}
}

func TestDecodeInboundMessage_EmptyPayload(t *testing.T) {
	t.Parallel()
	if _, err := engine.DecodeInboundMessage(nil); !errors.Is(err, engine.ErrInvalidEvent) {
		t.Errorf("Decode(nil) = %v, want ErrInvalidEvent", err)
	}
	if _, err := engine.DecodeInboundMessage([]byte("not-json")); !errors.Is(err, engine.ErrInvalidEvent) {
		t.Errorf("Decode(garbage) = %v, want ErrInvalidEvent", err)
	}
}

func mustReencode(t *testing.T, ev engine.InboundMessageEvent) []byte {
	t.Helper()
	// Use the same encoder via the InboundMessage variant when fields
	// are wholesale uuids; for hand-crafted broken cases we fall back
	// to json.Marshal directly via encoding/json.
	raw, err := jsonMarshal(ev)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return raw
}
