package nats_test

// SIN-62908 — embedded-JetStream integration tests for the
// aiassist_invalidator subscriber. Reuses runEmbedded + newJSContext
// helpers from sdk_embed_test.go and wallet_subscriber_test.go (same
// package nats_test, no duplicated bootstrap).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	natsgo "github.com/nats-io/nats.go"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	aiassistinvalidator "github.com/pericles-luz/crm/internal/worker/aiassist_invalidator"
)

const aiassistTestStream = "AIASSIST_INVALIDATOR_TEST"

// newAIAssistJS configures a JetStream stream bound to the
// message.created subject so the subscriber can consume real
// deliveries inside the embedded server.
func newAIAssistJS(t *testing.T, url string) natsgo.JetStreamContext {
	t.Helper()
	conn, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{
		Name:       aiassistTestStream,
		Subjects:   []string{aiassistinvalidator.SubjectMessageCreated},
		Storage:    natsgo.MemoryStorage,
		Retention:  natsgo.WorkQueuePolicy,
		Duplicates: time.Hour,
	}); err != nil {
		t.Fatalf("add stream: %v", err)
	}
	return js
}

func TestNewAIAssistInvalidatorSubscriber_RequiresJS(t *testing.T) {
	t.Parallel()
	if _, err := natsadapter.NewAIAssistInvalidatorSubscriber(natsadapter.AIAssistInvalidatorSubscriberConfig{}); err == nil {
		t.Fatal("expected error for missing JS")
	}
}

func TestNewAIAssistInvalidatorSubscriber_RejectsMaxDeliverShortcut(t *testing.T) {
	t.Parallel()
	url := runEmbedded(t)
	js := newAIAssistJS(t, url)
	// MaxDeliver must be > len(BackOff). 1 < 2 → reject.
	_, err := natsadapter.NewAIAssistInvalidatorSubscriber(natsadapter.AIAssistInvalidatorSubscriberConfig{
		JS:         js,
		Stream:     aiassistTestStream,
		MaxDeliver: 1,
		BackOff:    []time.Duration{1 * time.Second, 2 * time.Second},
	})
	if err == nil {
		t.Fatal("expected error when MaxDeliver <= len(BackOff)")
	}
}

func TestAIAssistInvalidatorSubscriber_DeliversAndAcks(t *testing.T) {
	t.Parallel()
	url := runEmbedded(t)
	js := newAIAssistJS(t, url)
	sub, err := natsadapter.NewAIAssistInvalidatorSubscriber(natsadapter.AIAssistInvalidatorSubscriberConfig{
		JS:      js,
		Stream:  aiassistTestStream,
		AckWait: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	deliveries, _, err := sub.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Publish a single message.created event.
	tenant, conv := uuid.New(), uuid.New()
	body, _ := json.Marshal(aiassistinvalidator.Event{
		TenantID:       tenant,
		ConversationID: conv,
		MessageID:      uuid.New(),
		CreatedAt:      time.Now().UTC(),
	})
	m := &natsgo.Msg{
		Subject: aiassistinvalidator.SubjectMessageCreated,
		Data:    body,
		Header:  natsgo.Header{},
	}
	m.Header.Set("Nats-Msg-Id", "aiassist-test-1")
	if _, err := js.PublishMsg(m); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case d, ok := <-deliveries:
		if !ok {
			t.Fatal("deliveries channel closed before delivery")
		}
		if d.MsgID() != "aiassist-test-1" {
			t.Errorf("msg id: got %q want aiassist-test-1", d.MsgID())
		}
		var got aiassistinvalidator.Event
		if err := json.Unmarshal(d.Data(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.TenantID != tenant || got.ConversationID != conv {
			t.Errorf("payload mismatch: got (%s, %s)", got.TenantID, got.ConversationID)
		}
		if err := d.Ack(ctx); err != nil {
			t.Errorf("Ack: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("delivery never arrived")
	}
}

func TestAIAssistInvalidatorSubscriber_NakWithDelay(t *testing.T) {
	t.Parallel()
	url := runEmbedded(t)
	js := newAIAssistJS(t, url)
	sub, err := natsadapter.NewAIAssistInvalidatorSubscriber(natsadapter.AIAssistInvalidatorSubscriberConfig{
		JS:      js,
		Stream:  aiassistTestStream,
		AckWait: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	deliveries, _, err := sub.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	tenant, conv := uuid.New(), uuid.New()
	body, _ := json.Marshal(aiassistinvalidator.Event{TenantID: tenant, ConversationID: conv})
	m := &natsgo.Msg{
		Subject: aiassistinvalidator.SubjectMessageCreated,
		Data:    body,
		Header:  natsgo.Header{},
	}
	m.Header.Set("Nats-Msg-Id", "aiassist-nak-1")
	if _, err := js.PublishMsg(m); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case d := <-deliveries:
		if err := d.Nak(ctx, 100*time.Millisecond); err != nil {
			t.Errorf("Nak: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("delivery never arrived")
	}

	// After the nak delay, the broker re-delivers the message.
	select {
	case d := <-deliveries:
		if d.MsgID() != "aiassist-nak-1" {
			t.Errorf("redelivery msg id: got %q", d.MsgID())
		}
		if err := d.Ack(ctx); err != nil {
			t.Errorf("Ack: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("redelivery never arrived")
	}
}
