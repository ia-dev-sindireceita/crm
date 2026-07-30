package usecase_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// TestSendOutbound_ThreadsIdempotencyKey proves the use case forwards
// SendOutboundInput.IdempotencyKey onto the OutboundMessage handed to the
// channel port (SIN-68306). The outbound dispatcher's idempotency
// decorator keys dedup on that field, so the plumbing must carry it.
func TestSendOutbound_ThreadsIdempotencyKey(t *testing.T) {
	repo := newInMemoryRepo()
	tenant := uuid.New()
	contact := uuid.New()
	conv, _ := inbox.NewConversation(tenant, contact, "whatsapp")
	if err := repo.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("seed CreateConversation: %v", err)
	}
	outbound := newStubOutbound("wamid.abc")
	u := inboxusecase.MustNewSendOutbound(repo, newStubWalletDebitor(), outbound)

	if _, err := u.Execute(context.Background(), inboxusecase.SendOutboundInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		Body:           "com chave",
		ToExternalID:   "+5511999990001",
		IdempotencyKey: "compose-42",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := outbound.Calls()
	if len(calls) != 1 {
		t.Fatalf("outbound calls = %d, want 1", len(calls))
	}
	if calls[0].IdempotencyKey != "compose-42" {
		t.Errorf("OutboundMessage.IdempotencyKey = %q, want compose-42", calls[0].IdempotencyKey)
	}
}

// TestSendOutbound_NoIdempotencyKey_IsEmpty confirms the default remains
// the pre-dispatcher behaviour: an unset key leaves the OutboundMessage
// key empty so dedup is disabled.
func TestSendOutbound_NoIdempotencyKey_IsEmpty(t *testing.T) {
	repo := newInMemoryRepo()
	tenant := uuid.New()
	conv, _ := inbox.NewConversation(tenant, uuid.New(), "whatsapp")
	if err := repo.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("seed CreateConversation: %v", err)
	}
	outbound := newStubOutbound("wamid.abc")
	u := inboxusecase.MustNewSendOutbound(repo, newStubWalletDebitor(), outbound)

	if _, err := u.Execute(context.Background(), inboxusecase.SendOutboundInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		Body:           "sem chave",
		ToExternalID:   "+5511999990002",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := outbound.Calls()[0].IdempotencyKey; got != "" {
		t.Errorf("OutboundMessage.IdempotencyKey = %q, want empty", got)
	}
}
