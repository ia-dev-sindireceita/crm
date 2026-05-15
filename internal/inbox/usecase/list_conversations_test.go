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

func TestNewListConversations_RejectsNilRepo(t *testing.T) {
	t.Parallel()
	if _, err := usecase.NewListConversations(nil); err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestMustNewListConversations_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic, got none")
		}
	}()
	_ = usecase.MustNewListConversations(nil)
}

func TestListConversations_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	uc := usecase.MustNewListConversations(repo)

	_, err := uc.Execute(context.Background(), usecase.ListConversationsInput{})
	if !errors.Is(err, inbox.ErrInvalidTenant) {
		t.Fatalf("err: got %v want %v", err, inbox.ErrInvalidTenant)
	}
}

func TestListConversations_FiltersByTenantAndState(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	tenantA := uuid.New()
	tenantB := uuid.New()

	mustSeedConv(t, repo, tenantA, "whatsapp", inbox.ConversationStateOpen, time.Now().Add(-2*time.Minute))
	mustSeedConv(t, repo, tenantA, "whatsapp", inbox.ConversationStateClosed, time.Now().Add(-1*time.Minute))
	mustSeedConv(t, repo, tenantB, "whatsapp", inbox.ConversationStateOpen, time.Now())

	uc := usecase.MustNewListConversations(repo)

	t.Run("tenantA open only", func(t *testing.T) {
		res, err := uc.Execute(context.Background(), usecase.ListConversationsInput{TenantID: tenantA, State: "open"})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if len(res.Items) != 1 {
			t.Fatalf("len: got %d items want 1: %+v", len(res.Items), res.Items)
		}
		if res.Items[0].State != "open" {
			t.Fatalf("state: got %q want open", res.Items[0].State)
		}
	})

	t.Run("tenantA both states (empty filter)", func(t *testing.T) {
		res, err := uc.Execute(context.Background(), usecase.ListConversationsInput{TenantID: tenantA})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if len(res.Items) != 2 {
			t.Fatalf("len: got %d items want 2", len(res.Items))
		}
	})

	t.Run("invalid state rejected", func(t *testing.T) {
		_, err := uc.Execute(context.Background(), usecase.ListConversationsInput{TenantID: tenantA, State: "garbage"})
		if !errors.Is(err, inbox.ErrInvalidStatus) {
			t.Fatalf("err: got %v want %v", err, inbox.ErrInvalidStatus)
		}
	})

	t.Run("closed filter", func(t *testing.T) {
		res, err := uc.Execute(context.Background(), usecase.ListConversationsInput{TenantID: tenantA, State: "closed"})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if len(res.Items) != 1 || res.Items[0].State != "closed" {
			t.Fatalf("got %+v", res.Items)
		}
	})
}

func TestListConversations_AppliesLimitDefault(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	tenant := uuid.New()
	for i := 0; i < 3; i++ {
		mustSeedConv(t, repo, tenant, "whatsapp", inbox.ConversationStateOpen, time.Now().Add(time.Duration(-i)*time.Minute))
	}
	uc := usecase.MustNewListConversations(repo)
	res, err := uc.Execute(context.Background(), usecase.ListConversationsInput{TenantID: tenant})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Items) != 3 {
		t.Fatalf("len: got %d want 3", len(res.Items))
	}
}

// mustSeedConv inserts a conversation directly into the in-memory repo
// for the test, bypassing constructor invariants we are not validating
// in the list path.
func mustSeedConv(t *testing.T, repo *inMemoryRepo, tenantID uuid.UUID, channel string, state inbox.ConversationState, lastMessageAt time.Time) *inbox.Conversation {
	t.Helper()
	c, err := inbox.NewConversation(tenantID, uuid.New(), channel)
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	switch state {
	case inbox.ConversationStateClosed:
		c.Close()
	}
	c.LastMessageAt = lastMessageAt
	if err := repo.CreateConversation(context.Background(), c); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	return c
}
