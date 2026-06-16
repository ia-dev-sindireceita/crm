package main

// SIN-63824 / SIN-63793 W5 — llmcustomer branch of buildInboxHandler.
//
// Wires the postgres-backed inbox storage to the fake-customer adapter
// from internal/adapter/channels/llmcustomer (W2) so an operator can
// exercise the full /inbox loop end-to-end in dev/staging without a
// real carrier. The adapter doubles as inbox.InboundChannel (downstream
// of NewReceiveInbound) and inbox.OutboundChannel (downstream of
// NewSendOutbound) — one adapter, both faces — so an operator reply is
// immediately followed (after llmcustomerReplyDelay) by a canned-LLM
// inbound that lands on the same conversation.
//
// Bootstrap policy is lazy and per-tenant: the first GET /inbox per
// (tenant, process) calls Adapter.Bootstrap, which is idempotent across
// the in-memory bootstrapped set AND the dedup ledger so concurrent
// requests / restarts collapse into a single synthetic conversation.
// Adopting the lazy hook keeps the wire free of an upfront tenant
// scan — the master-tenants list is owned by master_ops and tenants
// can be added at runtime, so a boot-time loop would miss them anyway.
//
// Fail-soft: DATABASE_URL unset OR any postgres construction error
// reverts to the disabled-mode stubs. That matches the rest of the
// cmd/server wires (web/contacts, web/funnel, etc.) and keeps the
// listener bootable when a smoke deploy lacks a database.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer"
	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer/canned"
	openrouterpersona "github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer/openrouter"
	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	"github.com/pericles-luz/crm/internal/contacts"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// llmcustomerReplyDelay is the wall-clock delay between the operator's
// outbound send and the scheduled customer inbound landing on the
// downstream inbox use case. 500ms is short enough that integration
// tests poll-and-finish quickly but long enough that the HTMX bubble
// renders the outbound message before the reply lands — matching the
// adapter doc-comment guidance (Config.ReplyDelay).
const llmcustomerReplyDelay = 500 * time.Millisecond

// fakellmRepository is the union port the llmcustomer wire wants from
// storage: every inbox.Repository method plus the dedup ledger ones.
// *pginbox.Store already satisfies both halves; declaring the union
// keeps the assembly function port-shaped (no concrete pg dependency)
// so tests can inject an in-memory fake without spinning up Postgres.
type fakellmRepository interface {
	inbox.Repository
	inbox.InboundDedupRepository
}

// inboxLLMCustomerDeps bundles the ports assembleInboxLLMCustomerHandler
// needs. Splitting this out from buildInboxHandlerLLMCustomer keeps the
// production "open pgxpool" path and the test "inject in-memory fakes"
// path on the same assembler — every code path the operator hits in
// production is exercised by the integration test.
type inboxLLMCustomerDeps struct {
	// Repo backs every inbox-side use case (list, get, save) AND the
	// dedup ledger the bootstrap inbound consults.
	Repo fakellmRepository
	// Contacts is the contacts port the upsert-by-channel use case
	// reads/writes.
	Contacts contacts.Repository
	// LLM produces the next customer line. nil falls back to the canned
	// default script so production wiring stays one-liner.
	LLM llmcustomer.PersonaLLM
	// ReplyDelay overrides llmcustomerReplyDelay. Tests set 0 so the
	// integration loop finishes in milliseconds.
	ReplyDelay time.Duration
	// Logger receives the bootstrap audit lines. nil falls back to
	// slog.Default with a "wire=inbox_llmcustomer" attribute.
	Logger *slog.Logger
}

// assembleInboxLLMCustomerHandler is the pure wireup: given already-built
// storage ports, return the http.Handler the chi router mounts plus a
// cleanup closure that stops the adapter. Returns an error rather than
// logging-and-skipping so the test path can assert failure modes; the
// production wrapper logs + falls back to disabled mode.
//
// The returned handler is the stdlib *http.ServeMux produced by
// webinbox.Handler.Routes — same shape buildInboxHandlerDisabled
// returns so cmd/server's mount path is uniform.
func assembleInboxLLMCustomerHandler(deps inboxLLMCustomerDeps) (http.Handler, func(), *llmcustomer.Adapter, error) {
	if deps.Repo == nil {
		return nil, nil, nil, errors.New("inbox/llmcustomer: Repo is required")
	}
	if deps.Contacts == nil {
		return nil, nil, nil, errors.New("inbox/llmcustomer: Contacts is required")
	}
	llm := deps.LLM
	if llm == nil {
		llm = canned.NewDefault()
	}
	delay := deps.ReplyDelay
	if delay < 0 {
		return nil, nil, nil, errors.New("inbox/llmcustomer: ReplyDelay must be >= 0")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("wire", "inbox_llmcustomer")

	contactsUC, err := contactsusecase.New(deps.Contacts)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("inbox/llmcustomer: contacts usecase: %w", err)
	}
	receiver, err := inboxusecase.NewReceiveInbound(deps.Repo, deps.Repo, contactsUC)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("inbox/llmcustomer: receive inbound usecase: %w", err)
	}

	adapter, err := llmcustomer.New(llmcustomer.Config{
		Downstream: receiver,
		LLM:        llm,
		ReplyDelay: delay,
		Logger:     logger,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("inbox/llmcustomer: adapter: %w", err)
	}

	listUC, err := inboxusecase.NewListConversations(deps.Repo)
	if err != nil {
		adapter.Stop()
		return nil, nil, nil, fmt.Errorf("inbox/llmcustomer: list conversations usecase: %w", err)
	}
	listMsgsUC, err := inboxusecase.NewListMessages(deps.Repo)
	if err != nil {
		adapter.Stop()
		return nil, nil, nil, fmt.Errorf("inbox/llmcustomer: list messages usecase: %w", err)
	}
	getMsgUC, err := inboxusecase.NewGetMessage(deps.Repo)
	if err != nil {
		adapter.Stop()
		return nil, nil, nil, fmt.Errorf("inbox/llmcustomer: get message usecase: %w", err)
	}
	// The fake adapter sends to the documented synthetic contact
	// regardless of the conversation's ContactID — supplying a fixed
	// lookup is cheaper than reading the contact-channel-identity row
	// and the recipient is contractually identical for every fakellm
	// conversation (one persona per tenant in v1, see persona_llm.go).
	syntheticLookup := func(context.Context, uuid.UUID, uuid.UUID) (string, error) {
		return llmcustomer.SyntheticContactExternalID, nil
	}
	sendUC, err := inboxusecase.NewSendOutbound(
		deps.Repo,
		llmcustomer.NewNoopWalletDebitor(),
		adapter,
		inboxusecase.WithContactLookup(syntheticLookup),
	)
	if err != nil {
		adapter.Stop()
		return nil, nil, nil, fmt.Errorf("inbox/llmcustomer: send outbound usecase: %w", err)
	}

	bootstrappedList := &bootstrapOnListConversations{
		inner:   listUC,
		adapter: adapter,
		logger:  logger,
	}

	// Conversation-context read feeds the real channel scope to the
	// AI-assist policy + customer panel (SIN-64969). Funnel readers are
	// nil here: this dev/staging loop wires no funnel storage, so the
	// stage fields degrade to zero-values (graceful) until the panel UI
	// ticket wires them. Channel + contact + assignment resolve fully.
	ctxUC, err := inboxusecase.NewGetConversationContext(deps.Repo, deps.Contacts, nil, nil)
	if err != nil {
		adapter.Stop()
		return nil, nil, nil, fmt.Errorf("inbox/llmcustomer: conversation context usecase: %w", err)
	}

	handlerDeps := webinbox.Deps{
		ListConversations:   bootstrappedList,
		ListMessages:        listMsgsUC,
		SendOutbound:        sendUC,
		GetMessage:          getMsgUC,
		ConversationContext: ctxUC,
		CSRFToken:           csrfTokenFromSessionContext,
		UserID:              userIDFromSessionContext,
		Logger:              logger,
	}
	h, err := webinbox.New(handlerDeps)
	if err != nil {
		adapter.Stop()
		return nil, nil, nil, fmt.Errorf("inbox/llmcustomer: webinbox.New: %w", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	cleanup := func() { adapter.Stop() }
	return mux, cleanup, adapter, nil
}

// buildInboxHandlerLLMCustomer is the production wrapper around
// assembleInboxLLMCustomerHandler. It opens DATABASE_URL, builds the
// pgx-backed inbox + contacts stores, and delegates the wiring. On any
// failure (missing DSN, connect error, assembly error) it falls back to
// the disabled-mode stubs so the route shell stays mounted and the
// boot remains soft-fail — consistent with the other web/* wires
// (web/contacts, web/funnel, web/aipolicy, etc.).
func buildInboxHandlerLLMCustomer(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: inbox handler degraded — provider=llmcustomer but DATABASE_URL unset; falling back to disabled stubs")
		return buildInboxHandlerDisabled()
	}
	pool, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: inbox handler degraded — provider=llmcustomer pg connect: %v; falling back to disabled stubs", err)
		return buildInboxHandlerDisabled()
	}
	mux, cleanup, err := assembleLLMCustomerFromPool(pool, getenv)
	if err != nil {
		pool.Close()
		log.Printf("crm: inbox handler degraded — provider=llmcustomer assemble: %v; falling back to disabled stubs", err)
		return buildInboxHandlerDisabled()
	}
	log.Printf("crm: inbox HTMX routes mounted on public listener (provider=llmcustomer, fake-customer adapter wired)")
	wrappedCleanup := func() {
		cleanup()
		pool.Close()
	}
	return mux, wrappedCleanup
}

// assembleLLMCustomerFromPool is a thin shim that converts a pgxpool
// into the postgres-backed ports the assembler expects. Split out so
// the test path that already owns a pool (or a fake) does not pay for
// the production "wire stores from pool" step.
//
// The persona LLM is selected via PERSONA_LLM_PROVIDER (canned vs.
// openrouter — see persona_llm_provider_wire.go). A nil LLM in the
// assembled deps falls back to the canned default; the openrouter
// branch supplies a constructed *openrouter.Persona so the operator
// loop drives a real model.
func assembleLLMCustomerFromPool(pool *pgxpool.Pool, getenv func(string) string) (http.Handler, func(), error) {
	inboxStore, err := pginbox.New(pool)
	if err != nil {
		return nil, nil, fmt.Errorf("pginbox.New: %w", err)
	}
	contactsStore, err := pgcontacts.New(pool)
	if err != nil {
		return nil, nil, fmt.Errorf("pgcontacts.New: %w", err)
	}
	personaLLM, err := buildPersonaLLM(getenv)
	if err != nil {
		return nil, nil, fmt.Errorf("persona-llm: %w", err)
	}
	mux, cleanup, _, err := assembleInboxLLMCustomerHandler(inboxLLMCustomerDeps{
		Repo:       inboxStore,
		Contacts:   contactsStore,
		LLM:        personaLLM,
		ReplyDelay: llmcustomerReplyDelay,
		// Logger left at the production default (slog.Default).
	})
	if err != nil {
		return nil, nil, err
	}
	return mux, cleanup, nil
}

// buildPersonaLLM resolves PERSONA_LLM_PROVIDER and constructs the
// matching PersonaLLM impl. The canned default is the safe fallback
// in dev/CI; openrouter wires the real chat-completion API client.
//
// A boot-time gate (PersonaLLMRefusedWithoutKey) already rejected the
// openrouter-without-key case before the listener bound, so reaching
// the openrouter branch here implies OPENROUTER_API_KEY is present —
// but openrouter.New is defensive and still re-checks, so a missing
// key fails loud at this layer too.
func buildPersonaLLM(getenv func(string) string) (llmcustomer.PersonaLLM, error) {
	provider, err := ReadPersonaLLMProvider(getenv)
	if err != nil {
		return nil, err
	}
	switch provider {
	case PersonaLLMProviderCanned:
		return canned.NewDefault(), nil
	case PersonaLLMProviderOpenRouter:
		key := ""
		model := ""
		if getenv != nil {
			key = getenv(envOpenRouterAPIKey)
			model = getenv(envPersonaLLMModel)
		}
		return openrouterpersona.New(openrouterpersona.Config{
			APIKey: key,
			Model:  model,
		})
	default:
		return nil, fmt.Errorf("unknown PERSONA_LLM_PROVIDER %q", provider)
	}
}

// bootstrapOnListConversations is the lazy-bootstrap decorator that
// fronts the production ListConversations use case. The first Execute
// per (tenant, process) calls adapter.Bootstrap so the synthetic
// conversation exists by the time the operator's HTMX response renders.
// Subsequent calls short-circuit on the in-memory once-per-tenant set;
// across process restarts the dedup ledger collapses repeat bootstrap
// inbounds to a single message (see llmcustomer.Adapter.Bootstrap).
//
// Bootstrap failures are logged at WARN and the underlying Execute
// still runs — degrading to an empty inbox is preferable to a 500 on
// a transient LLM hiccup; the next request retries.
type bootstrapOnListConversations struct {
	inner   *inboxusecase.ListConversations
	adapter *llmcustomer.Adapter
	logger  *slog.Logger

	mu   sync.Mutex
	seen map[uuid.UUID]struct{}
}

// Execute implements webinbox.ListConversationsUseCase. The bootstrap
// runs synchronously before the underlying list so the very first
// /inbox response sees the synthetic conversation already created
// (acceptance #1 of SIN-63824).
func (b *bootstrapOnListConversations) Execute(ctx context.Context, in inboxusecase.ListConversationsInput) (inboxusecase.ListConversationsResult, error) {
	if in.TenantID != uuid.Nil && b.markPending(in.TenantID) {
		if err := b.adapter.Bootstrap(ctx, in.TenantID); err != nil {
			logger := b.logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.WarnContext(ctx, "fakellm bootstrap deferred",
				"tenant_id", in.TenantID.String(),
				"err", err.Error(),
			)
			b.releasePending(in.TenantID)
		}
	}
	return b.inner.Execute(ctx, in)
}

// markPending records that tenantID is the first time we have seen
// it in this process. Returns true iff the caller now owns the
// bootstrap responsibility; concurrent callers see false and rely on
// the first caller's Bootstrap to land before they next list.
func (b *bootstrapOnListConversations) markPending(tenantID uuid.UUID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen == nil {
		b.seen = make(map[uuid.UUID]struct{})
	}
	if _, ok := b.seen[tenantID]; ok {
		return false
	}
	b.seen[tenantID] = struct{}{}
	return true
}

// releasePending undoes a markPending after a failed Bootstrap so the
// next list attempt retries. Bootstrap is idempotent on success
// (dedup ledger) so the only risk of leaving the mark in place is
// silently swallowing a transient LLM error on the very first call.
func (b *bootstrapOnListConversations) releasePending(tenantID uuid.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen == nil {
		return
	}
	delete(b.seen, tenantID)
}
