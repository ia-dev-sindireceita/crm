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

func TestNewListMessagesSince_RejectsNilRepo(t *testing.T) {
	t.Parallel()
	if _, err := usecase.NewListMessagesSince(nil); err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestMustNewListMessagesSince_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic, got none")
		}
	}()
	_ = usecase.MustNewListMessagesSince(nil)
}

func TestListMessagesSince_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	uc := usecase.MustNewListMessagesSince(newInMemoryRepo())
	_, err := uc.Execute(context.Background(), usecase.ListMessagesSinceInput{ConversationID: uuid.New()})
	if !errors.Is(err, inbox.ErrInvalidTenant) {
		t.Fatalf("err: got %v want %v", err, inbox.ErrInvalidTenant)
	}
}

func TestListMessagesSince_RejectsNilConversation(t *testing.T) {
	t.Parallel()
	uc := usecase.MustNewListMessagesSince(newInMemoryRepo())
	_, err := uc.Execute(context.Background(), usecase.ListMessagesSinceInput{TenantID: uuid.New()})
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("err: got %v want %v", err, inbox.ErrNotFound)
	}
}

func TestListMessagesSince_CrossTenantReturnsNotFound(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	tenantA := uuid.New()
	tenantB := uuid.New()
	conv := mustSeedConv(t, repo, tenantA, "fakellm", inbox.ConversationStateOpen, mustNow())
	uc := usecase.MustNewListMessagesSince(repo)
	_, err := uc.Execute(context.Background(), usecase.ListMessagesSinceInput{TenantID: tenantB, ConversationID: conv.ID})
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("err: got %v want ErrNotFound", err)
	}
}

// seedMsgAt persists a message with an explicit CreatedAt so the cursor
// arithmetic is deterministic (NewMessage stamps now()).
func seedMsgAt(t *testing.T, repo *inMemoryRepo, tenant, conv uuid.UUID, dir inbox.MessageDirection, body string, at time.Time) *inbox.Message {
	t.Helper()
	m, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID:       tenant,
		ConversationID: conv,
		Direction:      dir,
		Body:           body,
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	m.CreatedAt = at
	if err := repo.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	return m
}

func TestListMessagesSince_ZeroCursorReturnsAll(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	tenant := uuid.New()
	base := mustNow()
	conv := mustSeedConv(t, repo, tenant, "fakellm", inbox.ConversationStateOpen, base)
	seedMsgAt(t, repo, tenant, conv.ID, inbox.MessageDirectionOut, "oi", base)
	seedMsgAt(t, repo, tenant, conv.ID, inbox.MessageDirectionIn, "resposta", base.Add(time.Second))

	uc := usecase.MustNewListMessagesSince(repo)
	res, err := uc.Execute(context.Background(), usecase.ListMessagesSinceInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		AfterUnixNano:  0,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("len: got %d want 2 (zero cursor → all)", len(res.Items))
	}
}

func TestListMessagesSince_ReturnsOnlyStrictlyNewer(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	tenant := uuid.New()
	base := mustNow()
	conv := mustSeedConv(t, repo, tenant, "fakellm", inbox.ConversationStateOpen, base)
	out := seedMsgAt(t, repo, tenant, conv.ID, inbox.MessageDirectionOut, "pergunta", base)
	in := seedMsgAt(t, repo, tenant, conv.ID, inbox.MessageDirectionIn, "auto-reply", base.Add(time.Second))

	uc := usecase.MustNewListMessagesSince(repo)
	// Cursor at the outbound message → only the later inbound reply.
	res, err := uc.Execute(context.Background(), usecase.ListMessagesSinceInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		AfterUnixNano:  out.CreatedAt.UnixNano(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("len: got %d want 1", len(res.Items))
	}
	if res.Items[0].ID != in.ID || res.Items[0].Direction != "in" {
		t.Fatalf("item: got %+v want inbound %s", res.Items[0], in.ID)
	}
}

func TestListMessagesSince_RepollWithLatestCursorIsEmpty(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	tenant := uuid.New()
	base := mustNow()
	conv := mustSeedConv(t, repo, tenant, "fakellm", inbox.ConversationStateOpen, base)
	seedMsgAt(t, repo, tenant, conv.ID, inbox.MessageDirectionOut, "pergunta", base)
	last := seedMsgAt(t, repo, tenant, conv.ID, inbox.MessageDirectionIn, "auto-reply", base.Add(time.Second))

	uc := usecase.MustNewListMessagesSince(repo)
	// Re-poll exactly at the latest message → idempotent no-op (no dup).
	res, err := uc.Execute(context.Background(), usecase.ListMessagesSinceInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		AfterUnixNano:  last.CreatedAt.UnixNano(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("len: got %d want 0 (re-poll is idempotent)", len(res.Items))
	}
}
