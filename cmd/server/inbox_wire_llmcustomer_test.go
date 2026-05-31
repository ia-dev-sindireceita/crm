package main

// SIN-63824 / SIN-63793 W5 — selector wireup + llmcustomer integration tests.
//
// Two layers of coverage live in this file:
//
//   1. Selector dispatch: buildInboxHandler reads INBOX_CHANNEL_PROVIDER
//      and picks the right wire (disabled → stubs, llmcustomer → real
//      wire degraded to disabled when DATABASE_URL is unset, real →
//      nil + log). The dispatch tests cover the env permutations
//      without needing a database.
//
//   2. End-to-end loop: assembleInboxLLMCustomerHandler is exercised
//      against in-memory inbox/contacts stores so the integration test
//      asserts the operator→customer round-trip the SIN-63824 acceptance
//      criteria describe. The fakes mirror the postgres adapter's
//      contract; they DO NOT replace a database for any code path that
//      genuinely needs SQL — they let the use-case orchestration run
//      without Postgres because the tested invariant lives in the
//      use-case layer (the SIN-62729 PR4 fakes already use this same
//      pattern under internal/inbox/usecase).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer"
	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer/canned"
	"github.com/pericles-luz/crm/internal/contacts"
	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// --- Selector dispatch tests ---------------------------------------------

func TestBuildInboxHandler_Disabled_ReturnsStubHandler(t *testing.T) {
	t.Parallel()
	h, cleanup := buildInboxHandler(context.Background(), func(string) string { return "" })
	t.Cleanup(cleanup)
	if h == nil {
		t.Fatalf("buildInboxHandler returned nil for empty env; want stub mux")
	}
}

func TestBuildInboxHandler_DisabledExplicit_ReturnsStubHandler(t *testing.T) {
	t.Parallel()
	h, cleanup := buildInboxHandler(context.Background(), envOnly(map[string]string{
		envInboxChannelProvider: string(InboxChannelProviderDisabled),
	}))
	t.Cleanup(cleanup)
	if h == nil {
		t.Fatalf("buildInboxHandler returned nil for provider=disabled; want stub mux")
	}
}

func TestBuildInboxHandler_LLMCustomer_NoDSN_DegradesToStubs(t *testing.T) {
	t.Parallel()
	// Without DATABASE_URL the production wrapper falls back to the
	// disabled-mode stub mux so the route shell stays mounted. A nil
	// handler would make /inbox 404 silently — the SIN-63824 acceptance
	// is that the surface degrades to "empty inbox", not to "missing
	// page". See reference_crm_router_nil_dep_silent_skip in memory.
	h, cleanup := buildInboxHandler(context.Background(), envOnly(map[string]string{
		envInboxChannelProvider: string(InboxChannelProviderLLMCustomer),
	}))
	t.Cleanup(cleanup)
	if h == nil {
		t.Fatalf("buildInboxHandler returned nil for llmcustomer w/o DSN; want stub fallback")
	}
}

func TestBuildInboxHandler_Real_ReturnsNil(t *testing.T) {
	t.Parallel()
	// The "real" provider is intentionally not wired in W5 (deferred
	// to SIN-63793 W3). The selector MUST return a nil handler so the
	// chi router emits 404 for /inbox until the real-carrier wire
	// lands, not a half-wired surface.
	h, cleanup := buildInboxHandler(context.Background(), envOnly(map[string]string{
		envInboxChannelProvider: string(InboxChannelProviderReal),
	}))
	t.Cleanup(cleanup)
	if h != nil {
		t.Fatalf("buildInboxHandler returned non-nil for provider=real; want nil until W3")
	}
}

func TestBuildInboxHandler_UnknownProvider_ReturnsNil(t *testing.T) {
	t.Parallel()
	// W4's parser already refused "garbage" at boot. This test pins the
	// defensive fallback: if a typo somehow reaches the wire, the
	// listener still boots and the inbox stays unmounted.
	h, cleanup := buildInboxHandler(context.Background(), envOnly(map[string]string{
		envInboxChannelProvider: "garbage",
	}))
	t.Cleanup(cleanup)
	if h != nil {
		t.Fatalf("buildInboxHandler returned non-nil for provider=garbage; want nil")
	}
}

// --- Assembly contract tests ---------------------------------------------

func TestAssembleInboxLLMCustomerHandler_RejectsNilRepo(t *testing.T) {
	t.Parallel()
	_, _, _, err := assembleInboxLLMCustomerHandler(inboxLLMCustomerDeps{
		Contacts: newInMemoryContactsRepo(),
	})
	if err == nil {
		t.Fatal("expected error when Repo is nil")
	}
}

func TestAssembleInboxLLMCustomerHandler_RejectsNilContacts(t *testing.T) {
	t.Parallel()
	_, _, _, err := assembleInboxLLMCustomerHandler(inboxLLMCustomerDeps{
		Repo: newInMemoryInboxRepo(),
	})
	if err == nil {
		t.Fatal("expected error when Contacts is nil")
	}
}

func TestAssembleInboxLLMCustomerHandler_RejectsNegativeDelay(t *testing.T) {
	t.Parallel()
	_, _, _, err := assembleInboxLLMCustomerHandler(inboxLLMCustomerDeps{
		Repo:       newInMemoryInboxRepo(),
		Contacts:   newInMemoryContactsRepo(),
		ReplyDelay: -1,
	})
	if err == nil {
		t.Fatal("expected error when ReplyDelay is negative")
	}
}

// --- End-to-end loop integration test -----------------------------------

// TestLLMCustomerLoop_OperatorReplyTriggersFakeCustomerReply is the
// SIN-63824 acceptance integration test in code: with provider=
// llmcustomer the operator can list conversations (seeded via Bootstrap),
// open the synthetic conversation, post a reply, and observe the
// canned PersonaLLM's next line landing on the same conversation —
// all through the use cases the production handler dispatches into.
//
// The test goes through the assembled http.Handler.SendForView path on
// the SendOutbound use case (the same call the HTMX POST /inbox/
// conversations/{id}/messages handler makes) so the integration covers
// the exact code path operators hit, minus the HTTP layer + chi auth
// middleware that the higher-level web/inbox tests already pin.
func TestLLMCustomerLoop_OperatorReplyTriggersFakeCustomerReply(t *testing.T) {
	t.Parallel()
	repo := newInMemoryInboxRepo()
	contactsRepo := newInMemoryContactsRepo()
	tenantID := uuid.New()

	_, cleanup, adapter, err := assembleInboxLLMCustomerHandler(inboxLLMCustomerDeps{
		Repo:       repo,
		Contacts:   contactsRepo,
		LLM:        canned.NewDefault(),
		ReplyDelay: 0, // synchronous-looking: scheduler goroutine runs immediately.
	})
	if err != nil {
		t.Fatalf("assembleInboxLLMCustomerHandler: %v", err)
	}
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. Bootstrap: simulates the very first GET /inbox per tenant.
	//    The lazy-bootstrap decorator that fronts ListConversations in
	//    production calls Adapter.Bootstrap on the first request; we
	//    invoke it directly here to assert the side effect.
	if err := adapter.Bootstrap(ctx, tenantID); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// 2. List conversations — the synthetic conversation should be
	//    visible to the operator.
	listUC, err := inboxusecase.NewListConversations(repo)
	if err != nil {
		t.Fatalf("NewListConversations: %v", err)
	}
	list, err := listUC.Execute(ctx, inboxusecase.ListConversationsInput{
		TenantID: tenantID,
		State:    string(inbox.ConversationStateOpen),
	})
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("len(conversations)=%d, want 1 (synthetic from Bootstrap)", len(list.Items))
	}
	conversationID := list.Items[0].ID
	if list.Items[0].Channel != llmcustomer.ChannelName {
		t.Fatalf("conversation channel=%q, want %q", list.Items[0].Channel, llmcustomer.ChannelName)
	}

	// 3. Operator reply via the production-bound SendOutbound (this is
	//    the same use case the handler's send method drives). The
	//    no-op wallet adapter exercises the bookkeeping path uniformly
	//    per inbox.WalletDebitor contract.
	sendUC, err := inboxusecase.NewSendOutbound(
		repo,
		llmcustomer.NewNoopWalletDebitor(),
		adapter,
		inboxusecase.WithContactLookup(func(context.Context, uuid.UUID, uuid.UUID) (string, error) {
			return llmcustomer.SyntheticContactExternalID, nil
		}),
	)
	if err != nil {
		t.Fatalf("NewSendOutbound: %v", err)
	}
	view, err := sendUC.SendForView(ctx, inboxusecase.SendOutboundInput{
		TenantID:       tenantID,
		ConversationID: conversationID,
		Body:           "Olá, como posso ajudar?",
	})
	if err != nil {
		t.Fatalf("SendOutbound: %v", err)
	}
	if view.ID == uuid.Nil {
		t.Fatal("SendForView returned zero MessageID; want persisted outbound")
	}

	// 4. Wait for the scheduled customer reply to land. The reply
	//    goroutine runs through HandleInbound → ReceiveInbound →
	//    SaveMessage; once that happens the inbound message count for
	//    the conversation goes from 1 (bootstrap) to 2 (customer
	//    reply). The bounded poll fails the test if the adapter
	//    silently dropped the reply (LLM error, downstream rejection).
	adapter.Drain()

	listMsgs, err := inboxusecase.NewListMessages(repo)
	if err != nil {
		t.Fatalf("NewListMessages: %v", err)
	}
	msgs, err := listMsgs.Execute(ctx, inboxusecase.ListMessagesInput{
		TenantID:       tenantID,
		ConversationID: conversationID,
	})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	// Want: bootstrap-inbound + operator-outbound + customer-inbound = 3.
	if len(msgs.Items) != 3 {
		t.Fatalf("len(messages)=%d, want 3 (bootstrap + reply + customer); messages=%+v", len(msgs.Items), msgs.Items)
	}

	inbound := 0
	outbound := 0
	for _, m := range msgs.Items {
		switch m.Direction {
		case string(inbox.MessageDirectionIn):
			inbound++
		case string(inbox.MessageDirectionOut):
			outbound++
		}
	}
	if inbound != 2 {
		t.Errorf("inbound count=%d, want 2 (bootstrap + canned reply)", inbound)
	}
	if outbound != 1 {
		t.Errorf("outbound count=%d, want 1 (operator reply)", outbound)
	}

	// 5. GetMessage hits the same repo via the production read use
	//    case, mirroring the HTMX status-poll path. The operator's
	//    outbound row MUST be retrievable by its returned id.
	getMsg, err := inboxusecase.NewGetMessage(repo)
	if err != nil {
		t.Fatalf("NewGetMessage: %v", err)
	}
	got, err := getMsg.Execute(ctx, inboxusecase.GetMessageInput{
		TenantID:       tenantID,
		ConversationID: conversationID,
		MessageID:      view.ID,
	})
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Message.ID != view.ID {
		t.Fatalf("GetMessage.ID=%v, want %v", got.Message.ID, view.ID)
	}
}

// TestLLMCustomerBootstrap_IsIdempotentPerTenant pins the lazy-bootstrap
// decorator's contract: calling adapter.Bootstrap a second time for the
// same tenant collapses to a no-op so the operator never sees two
// synthetic conversations even when concurrent /inbox visits race.
func TestLLMCustomerBootstrap_IsIdempotentPerTenant(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	tenantID := uuid.New()
	for i := 0; i < 3; i++ {
		if err := adapter.Bootstrap(ctx, tenantID); err != nil {
			t.Fatalf("Bootstrap iter=%d: %v", i, err)
		}
	}
	if got := repo.conversationCount(); got != 1 {
		t.Fatalf("conversation count after 3 bootstraps = %d, want 1", got)
	}
}

// TestBootstrapOnListConversations_NilTenantSkipsBootstrap pins the
// safety contract: the decorator never calls Bootstrap when the input
// has no tenant id — that would land a stray uuid.Nil conversation on
// any tenant that ends up reading "the empty conversation".
func TestBootstrapOnListConversations_NilTenantSkipsBootstrap(t *testing.T) {
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
	listUC, err := inboxusecase.NewListConversations(repo)
	if err != nil {
		t.Fatalf("NewListConversations: %v", err)
	}
	decorator := &bootstrapOnListConversations{inner: listUC, adapter: adapter}
	// ListConversations rejects uuid.Nil with ErrInvalidTenant — we
	// expect the bootstrap to be skipped (no conversation seeded) and
	// the inner usecase's error to bubble through.
	_, err = decorator.Execute(context.Background(), inboxusecase.ListConversationsInput{
		TenantID: uuid.Nil,
	})
	if err == nil {
		t.Fatal("Execute returned nil; want ErrInvalidTenant")
	}
	if repo.conversationCount() != 0 {
		t.Fatalf("conversation count = %d, want 0 (no bootstrap on nil tenant)", repo.conversationCount())
	}
}

// TestBootstrapOnListConversations_RetryAfterBootstrapFailure pins the
// releasePending contract: when the first bootstrap LLM call errors
// the decorator clears its "seen" mark so the next request retries
// instead of silently serving an empty inbox forever.
func TestBootstrapOnListConversations_RetryAfterBootstrapFailure(t *testing.T) {
	t.Parallel()
	repo := newInMemoryInboxRepo()
	contactsRepo := newInMemoryContactsRepo()
	llm := &flakyPersonaLLM{}
	_, cleanup, adapter, err := assembleInboxLLMCustomerHandler(inboxLLMCustomerDeps{
		Repo:       repo,
		Contacts:   contactsRepo,
		LLM:        llm,
		ReplyDelay: 0,
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	t.Cleanup(cleanup)
	listUC, err := inboxusecase.NewListConversations(repo)
	if err != nil {
		t.Fatalf("NewListConversations: %v", err)
	}
	decorator := &bootstrapOnListConversations{inner: listUC, adapter: adapter}
	tenantID := uuid.New()
	// First call: LLM errors → bootstrap fails → releasePending clears
	// the in-flight mark → no conversation seeded → list still returns
	// the empty page.
	if _, err := decorator.Execute(context.Background(), inboxusecase.ListConversationsInput{
		TenantID: tenantID,
		State:    string(inbox.ConversationStateOpen),
	}); err != nil {
		t.Fatalf("Execute iter=1: %v", err)
	}
	if repo.conversationCount() != 0 {
		t.Fatalf("after failure: conversation count = %d, want 0", repo.conversationCount())
	}
	// Second call: LLM returns successfully → bootstrap proceeds →
	// synthetic conversation lands. Proves the decorator did NOT
	// short-circuit on the first failure.
	llm.SetReply("Oi, tudo bem?")
	if _, err := decorator.Execute(context.Background(), inboxusecase.ListConversationsInput{
		TenantID: tenantID,
		State:    string(inbox.ConversationStateOpen),
	}); err != nil {
		t.Fatalf("Execute iter=2: %v", err)
	}
	if repo.conversationCount() != 1 {
		t.Fatalf("after retry: conversation count = %d, want 1", repo.conversationCount())
	}
}

// flakyPersonaLLM is a PersonaLLM that errors until SetReply installs
// a successful reply. Tests use it to drive the bootstrap-failure /
// retry path in bootstrapOnListConversations.Execute.
type flakyPersonaLLM struct {
	mu    sync.Mutex
	reply string
}

func (f *flakyPersonaLLM) NextCustomerMessage(_ context.Context, _ string, _ []llmcustomer.Turn) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reply == "" {
		return "", errors.New("flakyPersonaLLM: synthetic error")
	}
	return f.reply, nil
}

func (f *flakyPersonaLLM) SetReply(reply string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = reply
}

// TestBootstrapOnListConversations_TriggersBootstrapOnce drives the
// lazy decorator (not the adapter directly) so the per-process "first
// list per tenant" branch is covered by an explicit test.
func TestBootstrapOnListConversations_TriggersBootstrapOnce(t *testing.T) {
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

	listUC, err := inboxusecase.NewListConversations(repo)
	if err != nil {
		t.Fatalf("NewListConversations: %v", err)
	}
	decorator := &bootstrapOnListConversations{
		inner:   listUC,
		adapter: adapter,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tenantID := uuid.New()
	for i := 0; i < 5; i++ {
		if _, err := decorator.Execute(ctx, inboxusecase.ListConversationsInput{
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

// --- httptest sanity check ----------------------------------------------

// TestLLMCustomerHandler_ServesRoutes pins that the assembled handler
// actually wires the /inbox/* route table — a regression here would
// mean the handler is built but unreachable, masking the loop failure
// as a 404 instead of a real error. The chi auth middleware that
// fronts the handler in production injects tenancy + CSRF context; we
// only verify the route is registered by checking that GET /inbox
// produces a non-404 (the missing tenancy context surfaces as 500,
// which is the expected boundary error and proves the route fired).
func TestLLMCustomerHandler_ServesRoutes(t *testing.T) {
	t.Parallel()
	repo := newInMemoryInboxRepo()
	contactsRepo := newInMemoryContactsRepo()
	handler, cleanup, _, err := assembleInboxLLMCustomerHandler(inboxLLMCustomerDeps{
		Repo:       repo,
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
		t.Fatalf("GET /inbox returned 404; route not mounted")
	}
}

// --- helpers -------------------------------------------------------------

// envOnly builds a getenv closure that returns values from the supplied
// map and the empty string for anything else. Mirrors the pattern in
// inbox_channel_provider_wire_test.go so the selector tests read like
// the W4 tests.
func envOnly(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

// --- in-memory fakes (lifted from internal/inbox/usecase/fakes_test.go) -
//
// These mirror the production postgres adapter's contract. They are
// kept in this _test.go file so the production binary never depends on
// them; the duplication is intentional per the SIN-63824 acceptance
// note ("no testcontainers required if W2's adapter is testable with a
// fake contact store") — we already had to maintain in-memory fakes
// at the use-case layer, and ratifying them at the cmd/server layer
// is the smallest verification that proves W5's wiring connects every
// port correctly.

type inMemoryInboxRepo struct {
	mu            sync.Mutex
	conversations map[uuid.UUID]*inbox.Conversation
	messages      map[uuid.UUID]*inbox.Message
	dedupClaimed  map[string]time.Time
	dedupDone     map[string]time.Time
}

func newInMemoryInboxRepo() *inMemoryInboxRepo {
	return &inMemoryInboxRepo{
		conversations: map[uuid.UUID]*inbox.Conversation{},
		messages:      map[uuid.UUID]*inbox.Message{},
		dedupClaimed:  map[string]time.Time{},
		dedupDone:     map[string]time.Time{},
	}
}

func (r *inMemoryInboxRepo) CreateConversation(_ context.Context, c *inbox.Conversation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.conversations[c.ID]; ok {
		return errors.New("inMemoryInboxRepo: duplicate conversation id")
	}
	cp := *c
	r.conversations[c.ID] = &cp
	return nil
}

func (r *inMemoryInboxRepo) GetConversation(_ context.Context, tenantID, conversationID uuid.UUID) (*inbox.Conversation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.conversations[conversationID]
	if !ok || c.TenantID != tenantID {
		return nil, inbox.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (r *inMemoryInboxRepo) FindOpenConversation(_ context.Context, tenantID, contactID uuid.UUID, channel string) (*inbox.Conversation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conversations {
		if c.TenantID == tenantID && c.ContactID == contactID && c.Channel == channel && c.State == inbox.ConversationStateOpen {
			cp := *c
			return &cp, nil
		}
	}
	return nil, inbox.ErrNotFound
}

func (r *inMemoryInboxRepo) SaveMessage(_ context.Context, m *inbox.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.messages[m.ID]; ok {
		return errors.New("inMemoryInboxRepo: duplicate message id")
	}
	cp := *m
	r.messages[m.ID] = &cp
	conv, ok := r.conversations[m.ConversationID]
	if !ok {
		return inbox.ErrNotFound
	}
	if m.CreatedAt.After(conv.LastMessageAt) {
		conv.LastMessageAt = m.CreatedAt
	}
	return nil
}

func (r *inMemoryInboxRepo) UpdateMessage(_ context.Context, m *inbox.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.messages[m.ID]; !ok {
		return inbox.ErrNotFound
	}
	cp := *m
	r.messages[m.ID] = &cp
	return nil
}

func (r *inMemoryInboxRepo) FindMessageByChannelExternalID(_ context.Context, tenantID uuid.UUID, channel, channelExternalID string) (*inbox.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.messages {
		if m.TenantID != tenantID || m.ChannelExternalID != channelExternalID {
			continue
		}
		conv, ok := r.conversations[m.ConversationID]
		if !ok || conv.Channel != channel {
			continue
		}
		cp := *m
		return &cp, nil
	}
	return nil, inbox.ErrNotFound
}

func (r *inMemoryInboxRepo) ListConversations(_ context.Context, tenantID uuid.UUID, state inbox.ConversationState, limit int) ([]*inbox.Conversation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*inbox.Conversation, 0)
	for _, c := range r.conversations {
		if c.TenantID != tenantID {
			continue
		}
		if state != "" && c.State != state {
			continue
		}
		cp := *c
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aj := out[i].LastMessageAt, out[j].LastMessageAt
		if ai.IsZero() {
			ai = out[i].CreatedAt
		}
		if aj.IsZero() {
			aj = out[j].CreatedAt
		}
		if !ai.Equal(aj) {
			return ai.After(aj)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *inMemoryInboxRepo) GetMessage(_ context.Context, tenantID, conversationID, messageID uuid.UUID) (*inbox.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.messages[messageID]
	if !ok || m.TenantID != tenantID || m.ConversationID != conversationID {
		return nil, inbox.ErrNotFound
	}
	cp := *m
	return &cp, nil
}

func (r *inMemoryInboxRepo) ListMessages(_ context.Context, tenantID, conversationID uuid.UUID) ([]*inbox.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	conv, ok := r.conversations[conversationID]
	if !ok || conv.TenantID != tenantID {
		return nil, inbox.ErrNotFound
	}
	out := make([]*inbox.Message, 0)
	for _, m := range r.messages {
		if m.TenantID != tenantID || m.ConversationID != conversationID {
			continue
		}
		cp := *m
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

func (r *inMemoryInboxRepo) Claim(_ context.Context, channel, externalID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := channel + "|" + externalID
	if _, ok := r.dedupClaimed[key]; ok {
		return inbox.ErrInboundAlreadyProcessed
	}
	r.dedupClaimed[key] = time.Now()
	return nil
}

func (r *inMemoryInboxRepo) MarkProcessed(_ context.Context, channel, externalID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := channel + "|" + externalID
	if _, ok := r.dedupClaimed[key]; !ok {
		return inbox.ErrNotFound
	}
	r.dedupDone[key] = time.Now()
	return nil
}

func (r *inMemoryInboxRepo) conversationCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conversations)
}

// inMemoryContactsRepo is a tiny contacts.Repository fake that mirrors
// the postgres adapter's "find by channel identity / save" contract.
type inMemoryContactsRepo struct {
	mu    sync.Mutex
	byID  map[uuid.UUID]*contacts.Contact
	owner map[string]uuid.UUID // channel|externalID → contact id
}

func newInMemoryContactsRepo() *inMemoryContactsRepo {
	return &inMemoryContactsRepo{
		byID:  map[uuid.UUID]*contacts.Contact{},
		owner: map[string]uuid.UUID{},
	}
}

func (r *inMemoryContactsRepo) Save(_ context.Context, c *contacts.Contact) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range c.Identities() {
		key := id.Channel + "|" + id.ExternalID
		if owner, ok := r.owner[key]; ok && owner != c.ID {
			return contacts.ErrChannelIdentityConflict
		}
	}
	r.byID[c.ID] = c
	for _, id := range c.Identities() {
		r.owner[id.Channel+"|"+id.ExternalID] = c.ID
	}
	return nil
}

func (r *inMemoryContactsRepo) FindByID(_ context.Context, tenantID, id uuid.UUID) (*contacts.Contact, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.byID[id]
	if !ok || c.TenantID != tenantID {
		return nil, contacts.ErrNotFound
	}
	return c, nil
}

func (r *inMemoryContactsRepo) FindByChannelIdentity(_ context.Context, tenantID uuid.UUID, channel, externalID string) (*contacts.Contact, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.owner[channel+"|"+externalID]
	if !ok {
		return nil, contacts.ErrNotFound
	}
	c, ok := r.byID[id]
	if !ok || c.TenantID != tenantID {
		return nil, contacts.ErrNotFound
	}
	return c, nil
}
