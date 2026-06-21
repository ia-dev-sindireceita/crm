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
// storage: every inbox.Repository method plus the dedup ledger, the
// assignment history ledger, and the conversation-lead cache.
// *pginbox.Store satisfies all four; declaring the union keeps the
// assembly function port-shaped (no concrete pg dependency) so tests
// can inject an in-memory fake without spinning up Postgres.
type fakellmRepository interface {
	inbox.Repository
	inbox.InboundDedupRepository
	inbox.AssignmentRepository
	inbox.ConversationLeadStore
	// ConversationStateStore backs the Encerrar / Reabrir conversa use
	// cases (SIN-65473): SetConversationState persists the lifecycle column.
	inbox.ConversationStateStore
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
	// ReadModel is the optional enriched read side (SIN-64967) that backs
	// the rich GET /inbox list (snippet + atendente + filters, SIN-64968).
	// nil falls back to the legacy channel+timestamp list. The postgres
	// path supplies the same *pginbox.Store, which satisfies both ports;
	// the in-memory test path may leave it nil.
	ReadModel inbox.ConversationReadModel
	// Directory is the optional user-label resolver for the assigned-
	// atendente chip (SIN-64967). nil leaves every row's assignee label
	// unresolved (rendered as "não atribuída" semantics handled upstream).
	Directory inbox.UserDirectory
	// Attendants backs the assignment use case (SIN-64979): IsAssignable
	// for the write path, ListAssignable for the dropdown. nil disables
	// the assign route and the interactive panel widget.
	Attendants inbox.AssignableAttendantRepository
	// Contacts is the contacts port the upsert-by-channel use case
	// reads/writes.
	Contacts contacts.Repository
	// LLM produces the next customer line. nil falls back to the canned
	// default script so production wiring stays one-liner.
	LLM llmcustomer.PersonaLLM
	// ReplyDelay overrides llmcustomerReplyDelay. Tests set 0 so the
	// integration loop finishes in milliseconds.
	ReplyDelay time.Duration
	// AIAssist carries the optional operator AI-assist collaborators
	// (SIN-65244). When AIAssist.Summarizer is nil the inbox handler
	// does not register POST /inbox/conversations/{id}/ai-assist and
	// does not render the "Resumir" button — the soft-degrade posture.
	// The production path fills Summarizer from buildAIAssistSummarizer
	// FromPool; the in-memory test path may leave it zero or inject a
	// fake to exercise route registration.
	AIAssist webinbox.AssistDeps
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
	// Live-thread refresh (SIN-65419): the incremental read side backing
	// GET .../messages/since. Reuses the same repository as ListMessages, so
	// wiring it adds no extra storage surface — it lets the open
	// conversation pane surface inbound auto-reply bubbles without a reload.
	listMsgsSinceUC, err := inboxusecase.NewListMessagesSince(deps.Repo)
	if err != nil {
		adapter.Stop()
		return nil, nil, nil, fmt.Errorf("inbox/llmcustomer: list messages since usecase: %w", err)
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

	// Enriched read side (SIN-64968). When a read model is wired the web
	// handler prefers it over ListConversations, so the lazy bootstrap
	// must fire on THIS path too — wrap it in the same bootstrap decorator
	// so the very first GET /inbox still seeds the synthetic conversation
	// before the rich list renders.
	var summaries webinbox.ListSummariesUseCase
	if deps.ReadModel != nil {
		summariesUC, err := inboxusecase.NewListConversationSummaries(deps.ReadModel, deps.Directory)
		if err != nil {
			adapter.Stop()
			return nil, nil, nil, fmt.Errorf("inbox/llmcustomer: list summaries usecase: %w", err)
		}
		summaries = &bootstrapOnListSummaries{
			inner:   summariesUC,
			adapter: adapter,
			logger:  logger,
		}
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

	// Assignment use case + dropdown (SIN-64979). Both are optional on
	// Deps: when Attendants is nil the route is not registered and the
	// panel degrades to read-only. The postgres *pginbox.Store satisfies
	// both inbox.AssignableAttendantRepository and the ledger/cache ports
	// required by NewAssignConversation — the same store instance backs
	// all three so there is no consistency gap.
	var assignUC webinbox.AssignConversationUseCase
	var listAssignableUC webinbox.ListAssignableUseCase
	if deps.Attendants != nil {
		assignUC = inboxusecase.MustNewAssignConversation(
			deps.Repo,
			deps.Repo,
			deps.Repo,
			deps.Attendants,
		)
		listAssignableUC = &listAssignableAdapter{r: deps.Attendants}
	}

	// Training-conversation reset (SIN-65392). The same fake adapter that
	// drives the auto-reply also satisfies inboxusecase.ConversationResetter
	// (clears per-tenant turn history + bootstrapped flag), so a reset
	// deletes the DB rows AND the in-memory simulator state in lock-step.
	// The use case rejects any non-fakellm conversation, so this is the
	// only inbox wire that registers the reset route.
	resetUC, err := inboxusecase.NewResetConversation(deps.Repo, adapter)
	if err != nil {
		adapter.Stop()
		return nil, nil, nil, fmt.Errorf("inbox/llmcustomer: reset conversation usecase: %w", err)
	}

	// Encerrar / Reabrir conversa (SIN-65473). Both read the conversation
	// under the tenant scope (deps.Repo) and persist the lifecycle column
	// through the same store's SetConversationState. They are wired as a
	// pair so a closed conversation always has a reopen path.
	closeUC, err := inboxusecase.NewCloseConversation(deps.Repo, deps.Repo)
	if err != nil {
		adapter.Stop()
		return nil, nil, nil, fmt.Errorf("inbox/llmcustomer: close conversation usecase: %w", err)
	}
	reopenUC, err := inboxusecase.NewReopenConversation(deps.Repo, deps.Repo)
	if err != nil {
		adapter.Stop()
		return nil, nil, nil, fmt.Errorf("inbox/llmcustomer: reopen conversation usecase: %w", err)
	}

	handlerDeps := webinbox.Deps{
		ListConversations:   bootstrappedList,
		ListSummaries:       summaries,
		ListMessages:        listMsgsUC,
		ListMessagesSince:   listMsgsSinceUC,
		SendOutbound:        sendUC,
		GetMessage:          getMsgUC,
		ConversationContext: ctxUC,
		AssignConversation:  assignUC,
		ListAssignable:      listAssignableUC,
		ResetConversation:   resetUC,
		CloseConversation:   closeUC,
		ReopenConversation:  reopenUC,
		AIAssist:            deps.AIAssist,
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
	userDir, err := pginbox.NewUserDirectory(pool)
	if err != nil {
		return nil, nil, fmt.Errorf("pginbox.NewUserDirectory: %w", err)
	}
	personaLLM, err := buildPersonaLLM(getenv)
	if err != nil {
		return nil, nil, fmt.Errorf("persona-llm: %w", err)
	}
	// Operator AI-assist (SIN-65244). Soft-degrade: a construction
	// fault disables the feature (Summarizer stays nil → route + button
	// off) but never downs the inbox. The (nil, nil) "key unset" case
	// is logged inside the builder.
	summarizer, err := buildAIAssistSummarizerFromPool(pool, getenv)
	if err != nil {
		log.Printf("crm: ai-assist operator summarizer disabled — assemble: %v; inbox continues without ai-assist", err)
		summarizer = nil
	}
	mux, cleanup, _, err := assembleInboxLLMCustomerHandler(inboxLLMCustomerDeps{
		Repo: inboxStore,
		// *pginbox.Store satisfies inbox.ConversationReadModel too, so the
		// same store backs the enriched GET /inbox list (SIN-64968).
		ReadModel: inboxStore,
		Directory: userDir,
		// *pginbox.Store satisfies inbox.AssignableAttendantRepository
		// (ListAssignable / IsAssignable) so the same store backs the
		// assignment use case and dropdown (SIN-64979).
		Attendants: inboxStore,
		Contacts:   contactsStore,
		LLM:        personaLLM,
		ReplyDelay: llmcustomerReplyDelay,
		AIAssist:   webinbox.AssistDeps{Summarizer: summarizer},
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
		if getenv != nil {
			key = getenv(envOpenRouterAPIKey)
		}
		// Model resolves through the unified knob (SIN-65244):
		// PERSONA_LLM_MODEL → OPENROUTER_MODEL → defaultLLMModel. The
		// persona client also falls back to its own DefaultModel const
		// when handed an empty string, but ReadPersonaModel never
		// returns empty, so the env-unset path now routes the persona
		// to the same shared default both LLM points use.
		return openrouterpersona.New(openrouterpersona.Config{
			APIKey: key,
			Model:  ReadPersonaModel(getenv),
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

// bootstrapOnListSummaries is the enriched-read-side twin of
// bootstrapOnListConversations (SIN-64968). When the web handler is wired
// with a ListSummaries dep it serves GET /inbox from the read model
// instead of ListConversations, so the lazy synthetic-conversation
// bootstrap must hang off THIS use case to keep the dev/staging loop
// self-seeding on the operator's first visit. Bootstrap is idempotent
// (in-memory once-per-tenant set + dedup ledger) so it stays a single
// synthetic conversation even though both decorators front the same
// adapter.
type bootstrapOnListSummaries struct {
	inner   *inboxusecase.ListConversationSummaries
	adapter *llmcustomer.Adapter
	logger  *slog.Logger

	mu   sync.Mutex
	seen map[uuid.UUID]struct{}
}

// Execute implements webinbox.ListSummariesUseCase. The bootstrap runs
// synchronously before the underlying list so the first /inbox response
// sees the synthetic conversation already created.
func (b *bootstrapOnListSummaries) Execute(ctx context.Context, in inboxusecase.ListConversationSummariesInput) (inboxusecase.ListConversationSummariesResult, error) {
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

// markPending records the first time tenantID is seen in this process,
// mirroring bootstrapOnListConversations.markPending. Returns true iff
// the caller now owns the bootstrap responsibility.
func (b *bootstrapOnListSummaries) markPending(tenantID uuid.UUID) bool {
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
// next list attempt retries.
func (b *bootstrapOnListSummaries) releasePending(tenantID uuid.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen == nil {
		return
	}
	delete(b.seen, tenantID)
}

// listAssignableAdapter adapts inbox.AssignableAttendantRepository to
// webinbox.ListAssignableUseCase so the composition root does not import
// the domain package from web/inbox (forbidwebboundary).
type listAssignableAdapter struct {
	r inbox.AssignableAttendantRepository
}

func (a *listAssignableAdapter) Execute(ctx context.Context, tenantID uuid.UUID) ([]webinbox.AssignableRow, error) {
	items, err := a.r.ListAssignable(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]webinbox.AssignableRow, len(items))
	for i, it := range items {
		out[i] = webinbox.AssignableRow{
			UserID:      it.UserID,
			DisplayName: it.DisplayName,
		}
	}
	return out, nil
}
