package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
	"github.com/pericles-luz/crm/internal/inbox/usecase"
)

func TestNewListMessages_RejectsNilRepo(t *testing.T) {
	t.Parallel()
	if _, err := usecase.NewListMessages(nil); err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestMustNewListMessages_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic, got none")
		}
	}()
	_ = usecase.MustNewListMessages(nil)
}

func TestListMessages_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	uc := usecase.MustNewListMessages(repo)
	_, err := uc.Execute(context.Background(), usecase.ListMessagesInput{ConversationID: uuid.New()})
	if !errors.Is(err, inbox.ErrInvalidTenant) {
		t.Fatalf("err: got %v want %v", err, inbox.ErrInvalidTenant)
	}
}

func TestListMessages_RejectsNilConversation(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	uc := usecase.MustNewListMessages(repo)
	_, err := uc.Execute(context.Background(), usecase.ListMessagesInput{TenantID: uuid.New()})
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("err: got %v want %v", err, inbox.ErrNotFound)
	}
}

func TestListMessages_ReturnsMessagesUnderTenantScope(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	tenant := uuid.New()
	conv := mustSeedConv(t, repo, tenant, "whatsapp", inbox.ConversationStateOpen, mustNow())

	msg, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		Direction:      inbox.MessageDirectionIn,
		Body:           "hello",
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if err := repo.SaveMessage(context.Background(), msg); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	uc := usecase.MustNewListMessages(repo)
	res, err := uc.Execute(context.Background(), usecase.ListMessagesInput{TenantID: tenant, ConversationID: conv.ID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("len: got %d want 1", len(res.Items))
	}
	if res.Items[0].Direction != "in" || res.Items[0].Body != "hello" {
		t.Fatalf("view: %+v", res.Items[0])
	}
}

func TestListMessages_CrossTenantReturnsNotFound(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	tenantA := uuid.New()
	tenantB := uuid.New()
	conv := mustSeedConv(t, repo, tenantA, "whatsapp", inbox.ConversationStateOpen, mustNow())
	uc := usecase.MustNewListMessages(repo)
	_, err := uc.Execute(context.Background(), usecase.ListMessagesInput{TenantID: tenantB, ConversationID: conv.ID})
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("err: got %v want ErrNotFound", err)
	}
}

func TestSendOutbound_SendForView_ReturnsViewOnSuccess(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	wallet := newStubWalletDebitor()
	out := newStubOutbound("wamid.test")
	tenant := uuid.New()
	conv := mustSeedConv(t, repo, tenant, "whatsapp", inbox.ConversationStateOpen, mustNow())

	uc := usecase.MustNewSendOutbound(repo, wallet, out)
	view, err := uc.SendForView(context.Background(), usecase.SendOutboundInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		Body:           "hello",
		ToExternalID:   "+5511999",
	})
	if err != nil {
		t.Fatalf("SendForView: %v", err)
	}
	if view.Direction != "out" {
		t.Fatalf("direction: got %q want out", view.Direction)
	}
	if view.Body != "hello" {
		t.Fatalf("body: got %q want hello", view.Body)
	}
	if view.Status != "sent" {
		t.Fatalf("status: got %q want sent", view.Status)
	}
}

func TestSendOutbound_SendForView_PropagatesError(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	wallet := newStubWalletDebitor()
	out := newStubOutbound("wamid.test")
	uc := usecase.MustNewSendOutbound(repo, wallet, out)
	_, err := uc.SendForView(context.Background(), usecase.SendOutboundInput{})
	if !errors.Is(err, inbox.ErrInvalidTenant) {
		t.Fatalf("err: got %v want %v", err, inbox.ErrInvalidTenant)
	}
}

func mustNow() time.Time { return time.Now().UTC() }
