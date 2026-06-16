package main

// SIN-64968 — coverage for the enriched-read-side (ListSummaries)
// wireup: the bootstrapOnListSummaries decorator and the
// assembleInboxLLMCustomerHandler branch that builds the summaries use
// case when a read model + directory are supplied. Uses the same
// in-memory fakes the W5 wire tests rely on plus a tiny read-model /
// directory pair (the in-memory inbox repo does not implement the
// CQRS read port, which is intentional — the production *pginbox.Store
// satisfies both).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// fakeReadModel is a minimal inbox.ConversationReadModel for the wire
// tests. It returns a fixed projection slice regardless of filter so the
// decorator / assembler branches can be exercised without Postgres.
type fakeReadModel struct {
	items []inbox.ConversationListItem
	err   error
}

func (f fakeReadModel) ListConversationSummaries(_ context.Context, _ uuid.UUID, _ inbox.ConversationFilter, _ int) ([]inbox.ConversationListItem, error) {
	return f.items, f.err
}

// fakeUserDir is a minimal inbox.UserDirectory returning a fixed label
// map.
type fakeUserDir struct {
	labels map[uuid.UUID]string
}

func (f fakeUserDir) LabelsByID(_ context.Context, _ uuid.UUID, _ []uuid.UUID) (map[uuid.UUID]string, error) {
	return f.labels, nil
}

// TestBootstrapOnListSummaries_TriggersBootstrapOnce mirrors the
// ListConversations decorator test: the first Execute per (tenant,
// process) seeds the synthetic conversation; subsequent calls short-
// circuit on the once-per-tenant set.
func TestBootstrapOnListSummaries_TriggersBootstrapOnce(t *testing.T) {
	t.Parallel()
	repo := newInMemoryInboxRepo()
	contactsRepo := newInMemoryContactsRepo()
	handler, cleanup, adapter, err := assembleInboxLLMCustomerHandler(inboxLLMCustomerDeps{
		Repo:       repo,
		Contacts:   contactsRepo,
		ReplyDelay: 0,
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	t.Cleanup(cleanup)
	_ = handler

	summariesUC, err := inboxusecase.NewListConversationSummaries(fakeReadModel{}, fakeUserDir{})
	if err != nil {
		t.Fatalf("NewListConversationSummaries: %v", err)
	}
	decorator := &bootstrapOnListSummaries{
		inner:   summariesUC,
		adapter: adapter,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tenantID := uuid.New()
	for i := 0; i < 5; i++ {
		if _, err := decorator.Execute(ctx, inboxusecase.ListConversationSummariesInput{
			TenantID: tenantID,
			State:    string(inbox.ConversationStateOpen),
		}); err != nil {
			t.Fatalf("Execute iter=%d: %v", i, err)
		}
	}
	if got := repo.conversationCount(); got != 1 {
		t.Fatalf("conversation count = %d, want 1 after 5 lazy bootstraps", got)
	}
}

// TestBootstrapOnListSummaries_NilTenantSkipsBootstrap pins that a
// uuid.Nil tenant never triggers a bootstrap (the use case rejects it
// with ErrInvalidTenant, and the decorator must not run Bootstrap for an
// invalid tenant).
func TestBootstrapOnListSummaries_NilTenantSkipsBootstrap(t *testing.T) {
	t.Parallel()
	repo := newInMemoryInboxRepo()
	contactsRepo := newInMemoryContactsRepo()
	_, cleanup, adapter, err := assembleInboxLLMCustomerHandler(inboxLLMCustomerDeps{
		Repo:       repo,
		Contacts:   contactsRepo,
		ReplyDelay: 0,
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	t.Cleanup(cleanup)

	summariesUC, err := inboxusecase.NewListConversationSummaries(fakeReadModel{}, fakeUserDir{})
	if err != nil {
		t.Fatalf("NewListConversationSummaries: %v", err)
	}
	decorator := &bootstrapOnListSummaries{inner: summariesUC, adapter: adapter}
	if _, err := decorator.Execute(context.Background(), inboxusecase.ListConversationSummariesInput{
		TenantID: uuid.Nil,
	}); err == nil {
		t.Fatalf("expected ErrInvalidTenant for nil tenant, got nil")
	}
	if got := repo.conversationCount(); got != 0 {
		t.Fatalf("conversation count = %d, want 0 (no bootstrap for nil tenant)", got)
	}
}

// TestAssembleInboxLLMCustomerHandler_WithReadModel_ServesRichList pins
// that supplying a read model + directory assembles the summaries use
// case and mounts the route. The chi auth middleware that injects
// tenancy is absent here, so GET /inbox surfaces the boundary 500 — a
// non-404 proves the route fired through the summaries-wired handler.
func TestAssembleInboxLLMCustomerHandler_WithReadModel_ServesRichList(t *testing.T) {
	t.Parallel()
	repo := newInMemoryInboxRepo()
	contactsRepo := newInMemoryContactsRepo()
	handler, cleanup, _, err := assembleInboxLLMCustomerHandler(inboxLLMCustomerDeps{
		Repo:       repo,
		ReadModel:  fakeReadModel{},
		Directory:  fakeUserDir{},
		Contacts:   contactsRepo,
		ReplyDelay: 0,
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	t.Cleanup(cleanup)

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/inbox")
	if err != nil {
		t.Fatalf("GET /inbox: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		t.Fatalf("GET /inbox returned 404; summaries-wired route not mounted")
	}
}
