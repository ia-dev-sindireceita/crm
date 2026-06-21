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

// stateRepo is a minimal fake satisfying the conversationReader +
// conversationStateWriter ports the close/reopen use cases depend on. It is
// NOT a database mock: it stores hydrated Conversation aggregates and lets
// SetConversationState mutate them, so the use-case orchestration (load
// under tenant scope → domain transition → persist) is exercised end to
// end without a live cluster.
type stateRepo struct {
	convs map[uuid.UUID]*inbox.Conversation
	// setErr, when non-nil, is returned by SetConversationState to exercise
	// the persistence-failure path.
	setErr error
	// setCalls records every (id,state) pair persisted, so a test can assert
	// the no-op path still reconciles the column (or, for reopen, skips it).
	setCalls []setCall
}

type setCall struct {
	id    uuid.UUID
	state inbox.ConversationState
}

func newStateRepo() *stateRepo {
	return &stateRepo{convs: map[uuid.UUID]*inbox.Conversation{}}
}

func (r *stateRepo) put(c *inbox.Conversation) { r.convs[c.ID] = c }

func (r *stateRepo) GetConversation(_ context.Context, tenantID, conversationID uuid.UUID) (*inbox.Conversation, error) {
	c, ok := r.convs[conversationID]
	if !ok || c.TenantID != tenantID {
		return nil, inbox.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (r *stateRepo) SetConversationState(_ context.Context, _, conversationID uuid.UUID, state inbox.ConversationState) error {
	if r.setErr != nil {
		return r.setErr
	}
	r.setCalls = append(r.setCalls, setCall{id: conversationID, state: state})
	if c, ok := r.convs[conversationID]; ok {
		c.State = state
	}
	return nil
}

func hydratedConv(t *testing.T, tenantID uuid.UUID, state inbox.ConversationState) *inbox.Conversation {
	t.Helper()
	return inbox.HydrateConversation(
		uuid.New(), tenantID, uuid.New(), "whatsapp", state, nil, time.Time{}, time.Time{},
	)
}

func TestNewCloseConversation_NilPorts(t *testing.T) {
	t.Parallel()
	repo := newStateRepo()
	if _, err := usecase.NewCloseConversation(nil, repo); err == nil {
		t.Fatal("nil reader: want error, got nil")
	}
	if _, err := usecase.NewCloseConversation(repo, nil); err == nil {
		t.Fatal("nil state writer: want error, got nil")
	}
	if _, err := usecase.NewCloseConversation(repo, repo); err != nil {
		t.Fatalf("valid ports: unexpected error %v", err)
	}
}

func TestCloseConversation_Execute(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()

	t.Run("closes an open conversation and persists the closed state", func(t *testing.T) {
		t.Parallel()
		repo := newStateRepo()
		conv := hydratedConv(t, tenant, inbox.ConversationStateOpen)
		repo.put(conv)
		uc := usecase.MustNewCloseConversation(repo, repo)

		res, err := uc.Execute(context.Background(), usecase.CloseConversationInput{
			TenantID:       tenant,
			ConversationID: conv.ID,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.AlreadyClosed {
			t.Fatal("AlreadyClosed=true for a previously-open conversation")
		}
		if got := repo.convs[conv.ID].State; got != inbox.ConversationStateClosed {
			t.Fatalf("stored state=%q, want closed", got)
		}
		if len(repo.setCalls) != 1 || repo.setCalls[0].state != inbox.ConversationStateClosed {
			t.Fatalf("setCalls=%+v, want one closed write", repo.setCalls)
		}
	})

	t.Run("closing an already-closed conversation is an idempotent no-op write", func(t *testing.T) {
		t.Parallel()
		repo := newStateRepo()
		conv := hydratedConv(t, tenant, inbox.ConversationStateClosed)
		repo.put(conv)
		uc := usecase.MustNewCloseConversation(repo, repo)

		res, err := uc.Execute(context.Background(), usecase.CloseConversationInput{
			TenantID:       tenant,
			ConversationID: conv.ID,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.AlreadyClosed {
			t.Fatal("AlreadyClosed=false for an already-closed conversation")
		}
		// Still reconciles the column (a drifted read-model is repaired).
		if len(repo.setCalls) != 1 {
			t.Fatalf("setCalls=%d, want 1 (idempotent re-persist)", len(repo.setCalls))
		}
	})

	t.Run("blocks further transfer once closed (aggregate invariant)", func(t *testing.T) {
		t.Parallel()
		repo := newStateRepo()
		conv := hydratedConv(t, tenant, inbox.ConversationStateOpen)
		repo.put(conv)
		uc := usecase.MustNewCloseConversation(repo, repo)
		if _, err := uc.Execute(context.Background(), usecase.CloseConversationInput{TenantID: tenant, ConversationID: conv.ID}); err != nil {
			t.Fatalf("close: %v", err)
		}
		// AssignTo on the now-closed aggregate must reject — the same gate the
		// transfer/assign use case honours (ErrConversationClosed).
		closed := repo.convs[conv.ID]
		if _, err := closed.AssignTo(uuid.New(), inbox.LeadReasonReassign); !errors.Is(err, inbox.ErrConversationClosed) {
			t.Fatalf("AssignTo on closed conversation err=%v, want ErrConversationClosed", err)
		}
	})

	t.Run("unknown id collapses to ErrNotFound (IDOR guard)", func(t *testing.T) {
		t.Parallel()
		repo := newStateRepo()
		uc := usecase.MustNewCloseConversation(repo, repo)
		_, err := uc.Execute(context.Background(), usecase.CloseConversationInput{TenantID: tenant, ConversationID: uuid.New()})
		if !errors.Is(err, inbox.ErrNotFound) {
			t.Fatalf("err=%v, want ErrNotFound", err)
		}
	})

	t.Run("cross-tenant id collapses to ErrNotFound", func(t *testing.T) {
		t.Parallel()
		repo := newStateRepo()
		conv := hydratedConv(t, tenant, inbox.ConversationStateOpen)
		repo.put(conv)
		uc := usecase.MustNewCloseConversation(repo, repo)
		_, err := uc.Execute(context.Background(), usecase.CloseConversationInput{TenantID: uuid.New(), ConversationID: conv.ID})
		if !errors.Is(err, inbox.ErrNotFound) {
			t.Fatalf("err=%v, want ErrNotFound", err)
		}
	})

	t.Run("nil tenant is rejected before any read", func(t *testing.T) {
		t.Parallel()
		repo := newStateRepo()
		uc := usecase.MustNewCloseConversation(repo, repo)
		_, err := uc.Execute(context.Background(), usecase.CloseConversationInput{TenantID: uuid.Nil, ConversationID: uuid.New()})
		if !errors.Is(err, inbox.ErrInvalidTenant) {
			t.Fatalf("err=%v, want ErrInvalidTenant", err)
		}
	})

	t.Run("nil conversation id is rejected", func(t *testing.T) {
		t.Parallel()
		repo := newStateRepo()
		uc := usecase.MustNewCloseConversation(repo, repo)
		_, err := uc.Execute(context.Background(), usecase.CloseConversationInput{TenantID: tenant, ConversationID: uuid.Nil})
		if !errors.Is(err, inbox.ErrNotFound) {
			t.Fatalf("err=%v, want ErrNotFound", err)
		}
	})

	t.Run("persistence failure surfaces", func(t *testing.T) {
		t.Parallel()
		repo := newStateRepo()
		conv := hydratedConv(t, tenant, inbox.ConversationStateOpen)
		repo.put(conv)
		repo.setErr = errors.New("boom")
		uc := usecase.MustNewCloseConversation(repo, repo)
		_, err := uc.Execute(context.Background(), usecase.CloseConversationInput{TenantID: tenant, ConversationID: conv.ID})
		if err == nil {
			t.Fatal("want persistence error, got nil")
		}
	})
}
