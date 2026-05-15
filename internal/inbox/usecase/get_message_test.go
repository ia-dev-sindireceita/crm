package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
	"github.com/pericles-luz/crm/internal/inbox/usecase"
)

func TestNewGetMessage_RejectsNilRepo(t *testing.T) {
	t.Parallel()
	if _, err := usecase.NewGetMessage(nil); err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestMustNewGetMessage_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic, got none")
		}
	}()
	_ = usecase.MustNewGetMessage(nil)
}

func TestGetMessage_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	uc := usecase.MustNewGetMessage(repo)
	_, err := uc.Execute(context.Background(), usecase.GetMessageInput{
		ConversationID: uuid.New(),
		MessageID:      uuid.New(),
	})
	if !errors.Is(err, inbox.ErrInvalidTenant) {
		t.Fatalf("err: got %v want %v", err, inbox.ErrInvalidTenant)
	}
}

func TestGetMessage_RejectsNilConversation(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	uc := usecase.MustNewGetMessage(repo)
	_, err := uc.Execute(context.Background(), usecase.GetMessageInput{
		TenantID:  uuid.New(),
		MessageID: uuid.New(),
	})
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("err: got %v want ErrNotFound", err)
	}
}

func TestGetMessage_RejectsNilMessageID(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	uc := usecase.MustNewGetMessage(repo)
	_, err := uc.Execute(context.Background(), usecase.GetMessageInput{
		TenantID:       uuid.New(),
		ConversationID: uuid.New(),
	})
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("err: got %v want ErrNotFound", err)
	}
}

func TestGetMessage_ReturnsViewUnderTenantScope(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	tenant := uuid.New()
	conv := mustSeedConv(t, repo, tenant, "whatsapp", inbox.ConversationStateOpen, mustNow())

	msg, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		Direction:      inbox.MessageDirectionOut,
		Body:           "ping",
		Status:         inbox.MessageStatusPending,
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if err := repo.SaveMessage(context.Background(), msg); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	uc := usecase.MustNewGetMessage(repo)
	res, err := uc.Execute(context.Background(), usecase.GetMessageInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		MessageID:      msg.ID,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Message.ID != msg.ID {
		t.Fatalf("id: got %s want %s", res.Message.ID, msg.ID)
	}
	if res.Message.Status != "pending" {
		t.Fatalf("status: got %q want pending", res.Message.Status)
	}
	if res.Message.Direction != "out" {
		t.Fatalf("direction: got %q want out", res.Message.Direction)
	}
}

func TestGetMessage_CrossTenantReturnsNotFound(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	tenantA := uuid.New()
	tenantB := uuid.New()
	conv := mustSeedConv(t, repo, tenantA, "whatsapp", inbox.ConversationStateOpen, mustNow())
	msg, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID:       tenantA,
		ConversationID: conv.ID,
		Direction:      inbox.MessageDirectionIn,
		Body:           "hi",
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if err := repo.SaveMessage(context.Background(), msg); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	uc := usecase.MustNewGetMessage(repo)
	_, err = uc.Execute(context.Background(), usecase.GetMessageInput{
		TenantID:       tenantB,
		ConversationID: conv.ID,
		MessageID:      msg.ID,
	})
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("err: got %v want ErrNotFound", err)
	}
}

func TestGetMessage_WrongConversationReturnsNotFound(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	tenant := uuid.New()
	conv := mustSeedConv(t, repo, tenant, "whatsapp", inbox.ConversationStateOpen, mustNow())
	msg, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		Direction:      inbox.MessageDirectionIn,
		Body:           "hi",
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if err := repo.SaveMessage(context.Background(), msg); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	uc := usecase.MustNewGetMessage(repo)
	_, err = uc.Execute(context.Background(), usecase.GetMessageInput{
		TenantID:       tenant,
		ConversationID: uuid.New(),
		MessageID:      msg.ID,
	})
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("err: got %v want ErrNotFound", err)
	}
}
