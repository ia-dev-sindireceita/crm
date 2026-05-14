package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

func TestNewSendOutbound_RejectsNilDeps(t *testing.T) {
	repo := newInMemoryRepo()
	wallet := newStubWalletDebitor()
	outbound := newStubOutbound("wamid.x")
	if _, err := inboxusecase.NewSendOutbound(nil, wallet, outbound); err == nil {
		t.Error("nil repo err = nil")
	}
	if _, err := inboxusecase.NewSendOutbound(repo, nil, outbound); err == nil {
		t.Error("nil wallet err = nil")
	}
	if _, err := inboxusecase.NewSendOutbound(repo, wallet, nil); err == nil {
		t.Error("nil outbound err = nil")
	}
}

func TestMustNewSendOutbound_PanicsOnNil(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("MustNewSendOutbound did not panic")
		}
	}()
	inboxusecase.MustNewSendOutbound(nil, nil, nil)
}

func TestSendOutbound_HappyPath_ZeroCost_DebitsWallet(t *testing.T) {
	repo := newInMemoryRepo()
	tenant := uuid.New()
	contact := uuid.New()
	conv, _ := inbox.NewConversation(tenant, contact, "whatsapp")
	if err := repo.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("seed CreateConversation: %v", err)
	}

	wallet := newStubWalletDebitor()
	outbound := newStubOutbound("wamid.123")
	u := inboxusecase.MustNewSendOutbound(repo, wallet, outbound)

	res, err := u.Execute(context.Background(), inboxusecase.SendOutboundInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		Body:           "hi back",
		ToExternalID:   "+5511999990001",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Message == nil {
		t.Fatal("Message = nil")
	}
	if res.Message.Status != inbox.MessageStatusSent {
		t.Errorf("Status = %q, want sent", res.Message.Status)
	}
	if res.Message.ChannelExternalID != "wamid.123" {
		t.Errorf("ChannelExternalID = %q, want wamid.123", res.Message.ChannelExternalID)
	}
	// AC #5 anchor: wallet.Debit MUST have been called even at cost=0.
	calls := wallet.Calls()
	if len(calls) != 1 || calls[0] != 0 {
		t.Errorf("wallet calls = %v, want [0]", calls)
	}
	if got := wallet.Balance(tenant); got != 0 {
		t.Errorf("balance = %d, want 0", got)
	}
	if len(outbound.Calls()) != 1 {
		t.Errorf("outbound calls = %d, want 1", len(outbound.Calls()))
	}
}

func TestSendOutbound_NonZeroCost_RefundsOnCarrierFailure(t *testing.T) {
	repo := newInMemoryRepo()
	tenant := uuid.New()
	conv, _ := inbox.NewConversation(tenant, uuid.New(), "whatsapp")
	if err := repo.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("seed: %v", err)
	}
	wallet := newStubWalletDebitor()
	wallet.Credit(tenant, 100)
	outbound := newStubOutbound("wamid.fails")
	outbound.SetError(errors.New("carrier rejected"))
	u := inboxusecase.MustNewSendOutbound(repo, wallet, outbound,
		inboxusecase.WithCost(func(_ context.Context, _ inbox.OutboundMessage) (int64, error) {
			return 5, nil
		}),
	)
	_, err := u.Execute(context.Background(), inboxusecase.SendOutboundInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		Body:           "hi",
		ToExternalID:   "+5511999990001",
	})
	if err == nil || err.Error() != "carrier rejected" {
		t.Fatalf("err = %v, want carrier rejected", err)
	}
	// On carrier failure the wallet must refund the reservation.
	if got := wallet.Balance(tenant); got != 100 {
		t.Errorf("balance after refund = %d, want 100", got)
	}
}

func TestSendOutbound_RejectsClosedConversation(t *testing.T) {
	repo := newInMemoryRepo()
	tenant := uuid.New()
	conv, _ := inbox.NewConversation(tenant, uuid.New(), "whatsapp")
	conv.Close()
	if err := repo.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := inboxusecase.MustNewSendOutbound(repo, newStubWalletDebitor(), newStubOutbound("x"))
	_, err := u.Execute(context.Background(), inboxusecase.SendOutboundInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		Body:           "hi",
		ToExternalID:   "+5511999990001",
	})
	if !errors.Is(err, inbox.ErrConversationClosed) {
		t.Errorf("err = %v, want ErrConversationClosed", err)
	}
}

func TestSendOutbound_RejectsZeroIDs(t *testing.T) {
	repo := newInMemoryRepo()
	u := inboxusecase.MustNewSendOutbound(repo, newStubWalletDebitor(), newStubOutbound("x"))
	if _, err := u.Execute(context.Background(), inboxusecase.SendOutboundInput{
		ConversationID: uuid.New(),
		Body:           "hi",
	}); !errors.Is(err, inbox.ErrInvalidTenant) {
		t.Errorf("zero tenant err = %v, want ErrInvalidTenant", err)
	}
	if _, err := u.Execute(context.Background(), inboxusecase.SendOutboundInput{
		TenantID: uuid.New(),
		Body:     "hi",
	}); !errors.Is(err, inbox.ErrInvalidContact) {
		t.Errorf("zero conversation err = %v, want ErrInvalidContact", err)
	}
}

func TestSendOutbound_PropagatesGetConversationError(t *testing.T) {
	repo := newInMemoryRepo()
	u := inboxusecase.MustNewSendOutbound(repo, newStubWalletDebitor(), newStubOutbound("x"))
	_, err := u.Execute(context.Background(), inboxusecase.SendOutboundInput{
		TenantID:       uuid.New(),
		ConversationID: uuid.New(),
		Body:           "hi",
	})
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSendOutbound_PropagatesCostFnError(t *testing.T) {
	repo := newInMemoryRepo()
	tenant := uuid.New()
	conv, _ := inbox.NewConversation(tenant, uuid.New(), "whatsapp")
	if err := repo.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("seed: %v", err)
	}
	wantErr := errors.New("cost broke")
	u := inboxusecase.MustNewSendOutbound(repo, newStubWalletDebitor(), newStubOutbound("x"),
		inboxusecase.WithCost(func(_ context.Context, _ inbox.OutboundMessage) (int64, error) {
			return 0, wantErr
		}),
	)
	_, err := u.Execute(context.Background(), inboxusecase.SendOutboundInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		Body:           "hi",
		ToExternalID:   "+5511999990001",
	})
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}

func TestSendOutbound_UsesContactLookupWhenToMissing(t *testing.T) {
	repo := newInMemoryRepo()
	tenant := uuid.New()
	conv, _ := inbox.NewConversation(tenant, uuid.New(), "whatsapp")
	if err := repo.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("seed: %v", err)
	}
	outbound := newStubOutbound("wamid.lookup")
	called := 0
	u := inboxusecase.MustNewSendOutbound(repo, newStubWalletDebitor(), outbound,
		inboxusecase.WithContactLookup(func(_ context.Context, tID, cID uuid.UUID) (string, error) {
			called++
			if tID != tenant || cID != conv.ID {
				t.Errorf("lookup args: %s/%s vs %s/%s", tID, cID, tenant, conv.ID)
			}
			return "+5511999990001", nil
		}),
	)
	if _, err := u.Execute(context.Background(), inboxusecase.SendOutboundInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		Body:           "hi",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if called != 1 {
		t.Errorf("contact lookup called = %d, want 1", called)
	}
	calls := outbound.Calls()
	if len(calls) != 1 || calls[0].ToExternalID != "+5511999990001" {
		t.Errorf("outbound calls = %+v", calls)
	}
}

func TestSendOutbound_FailedMessagePersisted(t *testing.T) {
	repo := newInMemoryRepo()
	tenant := uuid.New()
	conv, _ := inbox.NewConversation(tenant, uuid.New(), "whatsapp")
	if err := repo.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("seed: %v", err)
	}
	outbound := newStubOutbound("")
	outbound.SetError(errors.New("rejected"))
	u := inboxusecase.MustNewSendOutbound(repo, newStubWalletDebitor(), outbound)
	_, err := u.Execute(context.Background(), inboxusecase.SendOutboundInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		Body:           "hi",
		ToExternalID:   "+5511999990001",
	})
	if err == nil {
		t.Fatal("err = nil, want carrier failure")
	}
	if repo.messageCount() != 1 {
		t.Errorf("messages persisted = %d, want 1 (pending → failed)", repo.messageCount())
	}
}
