package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// recordingResetter records ResetConversation calls so the test can
// assert the channel adapter's in-memory state is reset in lock-step
// with the DB delete. An optional err exercises the propagation path.
type recordingResetter struct {
	calls  int
	tenant uuid.UUID
	conv   uuid.UUID
	err    error
}

func (r *recordingResetter) ResetConversation(_ context.Context, tenantID, conversationID uuid.UUID) error {
	r.calls++
	r.tenant = tenantID
	r.conv = conversationID
	return r.err
}

// seedConversationWithMessages creates a conversation on the given
// channel and saves n inbound messages so a reset has rows to delete.
func seedConversationWithMessages(t *testing.T, repo *inMemoryRepo, tenant, contact uuid.UUID, channel string, n int) uuid.UUID {
	t.Helper()
	conv, err := inbox.NewConversation(tenant, contact, channel)
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	if err := repo.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	for i := 0; i < n; i++ {
		m, err := inbox.NewMessage(inbox.NewMessageInput{
			TenantID:       tenant,
			ConversationID: conv.ID,
			Direction:      inbox.MessageDirectionIn,
			Body:           "msg",
		})
		if err != nil {
			t.Fatalf("NewMessage: %v", err)
		}
		if err := repo.SaveMessage(context.Background(), m); err != nil {
			t.Fatalf("SaveMessage: %v", err)
		}
	}
	return conv.ID
}

func TestNewResetConversation_RejectsNilRepo(t *testing.T) {
	t.Parallel()
	if _, err := inboxusecase.NewResetConversation(nil, inboxusecase.NoopConversationResetter{}); err == nil {
		t.Fatal("NewResetConversation(nil) err = nil, want error")
	}
}

func TestNewResetConversation_NilResetterDefaultsToNoop(t *testing.T) {
	t.Parallel()
	// A nil resetter must be tolerated (replaced with the no-op) so the
	// use case still runs the DB delete in deployments without a stateful
	// channel adapter.
	repo := newInMemoryRepo()
	uc, err := inboxusecase.NewResetConversation(repo, nil)
	if err != nil {
		t.Fatalf("NewResetConversation: %v", err)
	}
	tenant := uuid.New()
	conv := seedConversationWithMessages(t, repo, tenant, uuid.New(), inboxusecase.TrainingChannel, 2)
	res, err := uc.Execute(context.Background(), inboxusecase.ResetConversationInput{TenantID: tenant, ConversationID: conv})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Deleted != 2 {
		t.Fatalf("Deleted = %d, want 2", res.Deleted)
	}
}

func TestResetConversation_DeletesMessagesAndResetsAdapter(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	resetter := &recordingResetter{}
	uc := inboxusecase.MustNewResetConversation(repo, resetter)

	tenant := uuid.New()
	conv := seedConversationWithMessages(t, repo, tenant, uuid.New(), inboxusecase.TrainingChannel, 3)
	if got := repo.messageCount(); got != 3 {
		t.Fatalf("seeded messageCount = %d, want 3", got)
	}

	res, err := uc.Execute(context.Background(), inboxusecase.ResetConversationInput{TenantID: tenant, ConversationID: conv})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Deleted != 3 {
		t.Fatalf("Deleted = %d, want 3", res.Deleted)
	}
	if got := repo.messageCount(); got != 0 {
		t.Fatalf("post-reset messageCount = %d, want 0", got)
	}
	// Adapter state must be reset for the same (tenant, conversation).
	if resetter.calls != 1 {
		t.Fatalf("resetter calls = %d, want 1", resetter.calls)
	}
	if resetter.tenant != tenant || resetter.conv != conv {
		t.Fatalf("resetter args = (%v,%v), want (%v,%v)", resetter.tenant, resetter.conv, tenant, conv)
	}
}

func TestResetConversation_RejectsNonFakellmChannel(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	resetter := &recordingResetter{}
	uc := inboxusecase.MustNewResetConversation(repo, resetter)

	tenant := uuid.New()
	// A real customer conversation (whatsapp) must be refused, with NO
	// messages deleted and NO adapter reset — the blast-radius guard.
	conv := seedConversationWithMessages(t, repo, tenant, uuid.New(), "whatsapp", 2)

	_, err := uc.Execute(context.Background(), inboxusecase.ResetConversationInput{TenantID: tenant, ConversationID: conv})
	if !errors.Is(err, inboxusecase.ErrConversationNotResettable) {
		t.Fatalf("err = %v, want ErrConversationNotResettable", err)
	}
	if got := repo.messageCount(); got != 2 {
		t.Fatalf("messages deleted on a non-fakellm channel: count = %d, want 2 (untouched)", got)
	}
	if resetter.calls != 0 {
		t.Fatalf("adapter reset on a non-fakellm channel: calls = %d, want 0", resetter.calls)
	}
}

func TestResetConversation_UnknownConversationIsNotFound(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	uc := inboxusecase.MustNewResetConversation(repo, &recordingResetter{})
	_, err := uc.Execute(context.Background(), inboxusecase.ResetConversationInput{
		TenantID:       uuid.New(),
		ConversationID: uuid.New(),
	})
	if !errors.Is(err, inboxusecase.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResetConversation_IdempotentOnEmptyThread(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	resetter := &recordingResetter{}
	uc := inboxusecase.MustNewResetConversation(repo, resetter)

	tenant := uuid.New()
	conv := seedConversationWithMessages(t, repo, tenant, uuid.New(), inboxusecase.TrainingChannel, 0)

	res, err := uc.Execute(context.Background(), inboxusecase.ResetConversationInput{TenantID: tenant, ConversationID: conv})
	if err != nil {
		t.Fatalf("Execute on empty thread: %v", err)
	}
	if res.Deleted != 0 {
		t.Fatalf("Deleted = %d, want 0 on an empty thread", res.Deleted)
	}
	if resetter.calls != 1 {
		t.Fatalf("resetter calls = %d, want 1 (reset still runs on empty)", resetter.calls)
	}
}

func TestResetConversation_ValidatesInput(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	uc := inboxusecase.MustNewResetConversation(repo, &recordingResetter{})
	if _, err := uc.Execute(context.Background(), inboxusecase.ResetConversationInput{ConversationID: uuid.New()}); !errors.Is(err, inbox.ErrInvalidTenant) {
		t.Fatalf("nil tenant err = %v, want ErrInvalidTenant", err)
	}
	if _, err := uc.Execute(context.Background(), inboxusecase.ResetConversationInput{TenantID: uuid.New()}); !errors.Is(err, inboxusecase.ErrNotFound) {
		t.Fatalf("nil conversation err = %v, want ErrNotFound", err)
	}
}

func TestResetConversation_PropagatesResetterError(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	resetter := &recordingResetter{err: errors.New("adapter boom")}
	uc := inboxusecase.MustNewResetConversation(repo, resetter)
	tenant := uuid.New()
	conv := seedConversationWithMessages(t, repo, tenant, uuid.New(), inboxusecase.TrainingChannel, 1)
	if _, err := uc.Execute(context.Background(), inboxusecase.ResetConversationInput{TenantID: tenant, ConversationID: conv}); err == nil {
		t.Fatal("Execute err = nil, want resetter error propagated")
	}
}
