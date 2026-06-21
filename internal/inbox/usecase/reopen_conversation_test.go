package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
	"github.com/pericles-luz/crm/internal/inbox/usecase"
)

func TestNewReopenConversation_NilPorts(t *testing.T) {
	t.Parallel()
	repo := newStateRepo()
	if _, err := usecase.NewReopenConversation(nil, repo); err == nil {
		t.Fatal("nil reader: want error, got nil")
	}
	if _, err := usecase.NewReopenConversation(repo, nil); err == nil {
		t.Fatal("nil state writer: want error, got nil")
	}
	if _, err := usecase.NewReopenConversation(repo, repo); err != nil {
		t.Fatalf("valid ports: unexpected error %v", err)
	}
}

func TestReopenConversation_Execute(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()

	t.Run("reopens a closed conversation and persists the open state", func(t *testing.T) {
		t.Parallel()
		repo := newStateRepo()
		conv := hydratedConv(t, tenant, inbox.ConversationStateClosed)
		repo.put(conv)
		uc := usecase.MustNewReopenConversation(repo, repo)

		res, err := uc.Execute(context.Background(), usecase.ReopenConversationInput{
			TenantID:       tenant,
			ConversationID: conv.ID,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.AlreadyOpen {
			t.Fatal("AlreadyOpen=true for a previously-closed conversation")
		}
		if got := repo.convs[conv.ID].State; got != inbox.ConversationStateOpen {
			t.Fatalf("stored state=%q, want open", got)
		}
		if len(repo.setCalls) != 1 || repo.setCalls[0].state != inbox.ConversationStateOpen {
			t.Fatalf("setCalls=%+v, want one open write", repo.setCalls)
		}
	})

	t.Run("reopening an already-open conversation is an idempotent no-op (skips the write)", func(t *testing.T) {
		t.Parallel()
		repo := newStateRepo()
		conv := hydratedConv(t, tenant, inbox.ConversationStateOpen)
		repo.put(conv)
		uc := usecase.MustNewReopenConversation(repo, repo)

		res, err := uc.Execute(context.Background(), usecase.ReopenConversationInput{
			TenantID:       tenant,
			ConversationID: conv.ID,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.AlreadyOpen {
			t.Fatal("AlreadyOpen=false for an already-open conversation")
		}
		if len(repo.setCalls) != 0 {
			t.Fatalf("setCalls=%d, want 0 (no-op skips the write)", len(repo.setCalls))
		}
	})

	t.Run("close then reopen round-trips the lifecycle", func(t *testing.T) {
		t.Parallel()
		repo := newStateRepo()
		conv := hydratedConv(t, tenant, inbox.ConversationStateOpen)
		repo.put(conv)
		closeUC := usecase.MustNewCloseConversation(repo, repo)
		reopenUC := usecase.MustNewReopenConversation(repo, repo)

		if _, err := closeUC.Execute(context.Background(), usecase.CloseConversationInput{TenantID: tenant, ConversationID: conv.ID}); err != nil {
			t.Fatalf("close: %v", err)
		}
		if got := repo.convs[conv.ID].State; got != inbox.ConversationStateClosed {
			t.Fatalf("after close state=%q, want closed", got)
		}
		if _, err := reopenUC.Execute(context.Background(), usecase.ReopenConversationInput{TenantID: tenant, ConversationID: conv.ID}); err != nil {
			t.Fatalf("reopen: %v", err)
		}
		if got := repo.convs[conv.ID].State; got != inbox.ConversationStateOpen {
			t.Fatalf("after reopen state=%q, want open", got)
		}
		// Reopened conversation accepts a transfer again (the gate lifts).
		if _, err := repo.convs[conv.ID].AssignTo(uuid.New(), inbox.LeadReasonReassign); err != nil {
			t.Fatalf("AssignTo after reopen err=%v, want nil", err)
		}
	})

	t.Run("unknown id collapses to ErrNotFound", func(t *testing.T) {
		t.Parallel()
		repo := newStateRepo()
		uc := usecase.MustNewReopenConversation(repo, repo)
		_, err := uc.Execute(context.Background(), usecase.ReopenConversationInput{TenantID: tenant, ConversationID: uuid.New()})
		if !errors.Is(err, inbox.ErrNotFound) {
			t.Fatalf("err=%v, want ErrNotFound", err)
		}
	})

	t.Run("nil tenant is rejected", func(t *testing.T) {
		t.Parallel()
		repo := newStateRepo()
		uc := usecase.MustNewReopenConversation(repo, repo)
		_, err := uc.Execute(context.Background(), usecase.ReopenConversationInput{TenantID: uuid.Nil, ConversationID: uuid.New()})
		if !errors.Is(err, inbox.ErrInvalidTenant) {
			t.Fatalf("err=%v, want ErrInvalidTenant", err)
		}
	})

	t.Run("nil conversation id is rejected", func(t *testing.T) {
		t.Parallel()
		repo := newStateRepo()
		uc := usecase.MustNewReopenConversation(repo, repo)
		_, err := uc.Execute(context.Background(), usecase.ReopenConversationInput{TenantID: tenant, ConversationID: uuid.Nil})
		if !errors.Is(err, inbox.ErrNotFound) {
			t.Fatalf("err=%v, want ErrNotFound", err)
		}
	})

	t.Run("persistence failure surfaces", func(t *testing.T) {
		t.Parallel()
		repo := newStateRepo()
		conv := hydratedConv(t, tenant, inbox.ConversationStateClosed)
		repo.put(conv)
		repo.setErr = errors.New("boom")
		uc := usecase.MustNewReopenConversation(repo, repo)
		_, err := uc.Execute(context.Background(), usecase.ReopenConversationInput{TenantID: tenant, ConversationID: conv.ID})
		if err == nil {
			t.Fatal("want persistence error, got nil")
		}
	})
}
