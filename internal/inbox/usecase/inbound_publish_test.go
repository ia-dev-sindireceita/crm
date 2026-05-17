package usecase_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// recordingPublisher spies on PublishInboundMessage calls. Mirrors
// the recordingLinker pattern in campaign_link_test.go.
type recordingPublisher struct {
	mu    sync.Mutex
	calls []inboxusecase.PublishedInboundMessage
	err   error
}

func (p *recordingPublisher) PublishInboundMessage(_ context.Context, msg inboxusecase.PublishedInboundMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, msg)
	return p.err
}

func (p *recordingPublisher) snapshot() []inboxusecase.PublishedInboundMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]inboxusecase.PublishedInboundMessage, len(p.calls))
	copy(out, p.calls)
	return out
}

func newReceiveInboundForPublishTest(t *testing.T) (*inboxusecase.ReceiveInbound, uuid.UUID) {
	t.Helper()
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	uc := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)
	uc.SetInboundMessagePublisherLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	return uc, uuid.New()
}

func publishEvent(tenant uuid.UUID, body string) inbox.InboundEvent {
	return inbox.InboundEvent{
		TenantID:          tenant,
		Channel:           "webchat",
		ChannelExternalID: "ch-" + uuid.NewString(),
		SenderExternalID:  "+5511999990002",
		SenderDisplayName: "Bob",
		Body:              body,
	}
}

func TestReceiveInbound_PublishesAfterPersist(t *testing.T) {
	t.Parallel()
	uc, tenant := newReceiveInboundForPublishTest(t)
	pub := &recordingPublisher{}
	uc.SetInboundMessagePublisher(pub)

	res, err := uc.Execute(context.Background(), publishEvent(tenant, "olá tudo bem?"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Duplicate {
		t.Fatal("Execute reported duplicate on a fresh delivery")
	}
	calls := pub.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 publish call, got %d", len(calls))
	}
	got := calls[0]
	if got.TenantID != tenant {
		t.Errorf("TenantID = %v, want %v", got.TenantID, tenant)
	}
	if got.MessageID != res.Message.ID {
		t.Errorf("MessageID = %v, want %v", got.MessageID, res.Message.ID)
	}
	if got.ConversationID != res.Conversation.ID {
		t.Errorf("ConversationID = %v, want %v", got.ConversationID, res.Conversation.ID)
	}
	if got.Channel != "webchat" {
		t.Errorf("Channel = %q, want webchat", got.Channel)
	}
	if got.Body != "olá tudo bem?" {
		t.Errorf("Body = %q, want %q", got.Body, "olá tudo bem?")
	}
	if got.OccurredAt.IsZero() {
		t.Error("OccurredAt is zero, want message.CreatedAt")
	}
}

func TestReceiveInbound_PublisherErrorDoesNotAbortDelivery(t *testing.T) {
	t.Parallel()
	uc, tenant := newReceiveInboundForPublishTest(t)
	pub := &recordingPublisher{err: errors.New("nats unavailable")}
	uc.SetInboundMessagePublisher(pub)

	res, err := uc.Execute(context.Background(), publishEvent(tenant, "olá"))
	if err != nil {
		t.Fatalf("Execute: want soft-fail success, got err = %v", err)
	}
	if res.Message == nil {
		t.Fatal("Execute: message was not persisted on publisher failure")
	}
	if len(pub.snapshot()) != 1 {
		t.Errorf("publisher was not consulted on err path")
	}
}

func TestReceiveInbound_NoPublisherWiredIsNoop(t *testing.T) {
	t.Parallel()
	uc, tenant := newReceiveInboundForPublishTest(t)
	// No SetInboundMessagePublisher call.

	res, err := uc.Execute(context.Background(), publishEvent(tenant, "olá"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Message == nil {
		t.Fatal("Execute: message was not persisted with nil publisher")
	}
}

func TestReceiveInbound_DuplicateDoesNotPublish(t *testing.T) {
	t.Parallel()
	uc, tenant := newReceiveInboundForPublishTest(t)
	pub := &recordingPublisher{}
	uc.SetInboundMessagePublisher(pub)

	ev := publishEvent(tenant, "olá")
	if _, err := uc.Execute(context.Background(), ev); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if _, err := uc.Execute(context.Background(), ev); err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if len(pub.snapshot()) != 1 {
		t.Errorf("duplicate delivery republished: got %d calls, want 1", len(pub.snapshot()))
	}
}

func TestSetInboundMessagePublisherNilIsNoop(t *testing.T) {
	t.Parallel()
	uc, tenant := newReceiveInboundForPublishTest(t)
	uc.SetInboundMessagePublisher(nil)
	if _, err := uc.Execute(context.Background(), publishEvent(tenant, "olá")); err != nil {
		t.Errorf("Execute after SetInboundMessagePublisher(nil) failed: %v", err)
	}
}
