package main

// SIN-63824 / SIN-63793 W5 — operator inbox HTMX selector wireup.
//
// Composition root for /inbox. Reads INBOX_CHANNEL_PROVIDER (parsed and
// validated by inbox_channel_provider_wire.go / W4) and assembles the
// correct adapter family before mounting the route shell from W1:
//
//   - disabled   → stub use cases so the surface mounts but every
//                  endpoint surfaces empty-list / 404 cleanly. Same
//                  shape the W1 placeholder shipped.
//   - llmcustomer → real wireup: postgres-backed inbox.Store +
//                   contacts.Store + llmcustomer.Adapter (canned
//                   PersonaLLM) + NoopWalletDebitor. Bootstraps a
//                   synthetic conversation lazily on each first
//                   tenant-scoped GET /inbox so dev/staging operators
//                   see a working loop without a real carrier. Lives
//                   in inbox_wire_llmcustomer.go.
//   - real        → real-carrier wireup (SIN-67470 / W3): postgres-backed
//                   inbox read path + the WhatsApp Cloud outbound
//                   dispatcher (SIN-68306). Surfaces the inbound messages
//                   whatsapp_wire.go persists. Lives in
//                   inbox_wire_real.go. Fail-soft to disabled stubs when
//                   DATABASE_URL is unset.
//
// The handler.New constructor rejects nil required deps, so the
// disabled branch supplies tiny in-process stubs rather than guarding
// the route mount on a nil dep — that keeps the chi route table stable
// across the W2-W5 rollout (operators never see a regression from
// "404" → "200 empty" → "200 with data", just two state transitions).

import (
	"context"
	"log"
	"log/slog"
	"net/http"

	inboxdomain "github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// buildInboxHandler returns the /inbox HTMX mux + a cleanup closure.
// The returned http.Handler is the stdlib *http.ServeMux produced by
// webinbox.Handler.Routes; cmd/server hands it to httpapi.NewRouter via
// Deps.WebInbox so chi wraps it with TenantScope + Auth + CSRF +
// RequireAuth + RequireAction(iam.ActionTenantInboxRead) before
// dispatch.
func buildInboxHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	noop := func() {}
	provider, err := ReadInboxChannelProvider(getenv)
	if err != nil {
		// W4's parser already refused at boot via
		// InboxChannelProviderRefusedInProd; the only way this branch
		// fires here is a typo that slipped past the boot gate (e.g. a
		// test invokes the wire directly with a bogus env). Skip the
		// mount so the listener stays bootable but the operator sees a
		// 404 instead of a half-wired surface.
		log.Printf("crm: inbox handler disabled — %v", err)
		return nil, noop
	}
	switch provider {
	case InboxChannelProviderDisabled:
		return buildInboxHandlerDisabledWithOutbound(ctx, getenv)
	case InboxChannelProviderLLMCustomer:
		return buildInboxHandlerLLMCustomer(ctx, getenv)
	case InboxChannelProviderReal:
		return buildInboxHandlerReal(ctx, getenv)
	default:
		log.Printf("crm: inbox handler disabled — provider %q is not recognised", provider)
		return nil, noop
	}
}

// buildInboxHandlerDisabled mounts the inbox route shell with stub use
// cases (GET /inbox renders the empty-inbox shell, every other endpoint
// surfaces 404). This is the production-safe default; cmd/server keeps
// it whenever INBOX_CHANNEL_PROVIDER is unset or explicitly disabled so
// real-carrier work in SIN-63793 W3 has a route table to slot into.
func buildInboxHandlerDisabled() (http.Handler, func()) {
	return mountDisabledInbox(notFoundSendOutbound{}, func() {})
}

// buildInboxHandlerDisabledWithOutbound is the disabled provider path used
// by buildInboxHandler. It keeps the disabled read side (empty list / 404)
// but wires the real WhatsApp Graph outbound send path (SIN-68302) onto the
// POST /inbox/.../messages route when META_GRAPH_TOKEN + FEATURE_WHATSAPP +
// DATABASE_URL are all present. Deny-by-default: any gate closed keeps the
// notFoundSendOutbound{} stub, so the production default (bare deploy) is
// byte-for-byte the prior behaviour.
//
// The read side stays stubbed on purpose: mounting the postgres-backed
// conversation list + realtime read path is SIN-63793 W3's job. This branch
// closes only the outbound (send) half so an authenticated operator reply on
// a WhatsApp conversation dispatches through the Graph Sender instead of
// 404'ing (parent SIN-68301, Parte E two-way).
func buildInboxHandlerDisabledWithOutbound(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	send, sendCleanup, ok := buildInboxOutboundSendForView(ctx, getenv)
	if !ok {
		return buildInboxHandlerDisabled()
	}
	return mountDisabledInbox(send, sendCleanup)
}

// mountDisabledInbox assembles the disabled-mode route shell with the
// supplied outbound send use case (the stub or the real WhatsApp dispatcher)
// and returns the mux plus a cleanup that runs sendCleanup. Splitting the
// Deps construction out keeps the stub path and the outbound-wired path on
// one assembler so the route table is identical across both.
func mountDisabledInbox(send webinbox.SendOutboundUseCase, sendCleanup func()) (http.Handler, func()) {
	if sendCleanup == nil {
		sendCleanup = func() {}
	}
	deps := webinbox.Deps{
		ListConversations: emptyListConversations{},
		ListMessages:      notFoundListMessages{},
		SendOutbound:      send,
		GetMessage:        notFoundGetMessage{},
		CSRFToken:         csrfTokenFromSessionContext,
		UserID:            userIDFromSessionContext,
		Logger:            slog.Default(),
	}
	h, err := webinbox.New(deps)
	if err != nil {
		// New only fails when a required dep is nil; every field above
		// is non-nil so this branch is unreachable. Log + skip the
		// mount if a future refactor breaks the invariant — preserving
		// fail-soft boot behaviour.
		log.Printf("crm: inbox handler disabled — webinbox.New: %v", err)
		sendCleanup()
		return nil, func() {}
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	log.Printf("crm: inbox HTMX routes mounted on public listener (provider=disabled, stub deps)")
	return mux, sendCleanup
}

// emptyListConversations is the disabled-mode placeholder for the
// read-side that backs GET /inbox. Execute returns an empty Items slice
// for any tenant so the handler renders the empty-inbox shell (left
// list = empty, right pane = empty). The llmcustomer branch swaps it
// for the postgres-backed use case wrapped in a bootstrap decorator.
type emptyListConversations struct{}

func (emptyListConversations) Execute(_ context.Context, _ inboxusecase.ListConversationsInput) (inboxusecase.ListConversationsResult, error) {
	return inboxusecase.ListConversationsResult{Items: nil}, nil
}

// notFoundListMessages is the disabled-mode placeholder for GET
// /inbox/conversations/{id}. With no conversations seeded the handler
// MUST surface 404 on any direct visit; ErrNotFound is the documented
// signal the handler converts to http.StatusNotFound.
type notFoundListMessages struct{}

func (notFoundListMessages) Execute(_ context.Context, _ inboxusecase.ListMessagesInput) (inboxusecase.ListMessagesResult, error) {
	return inboxusecase.ListMessagesResult{}, inboxdomain.ErrNotFound
}

// notFoundSendOutbound is the disabled-mode placeholder for POST
// /inbox/conversations/{id}/messages. Without an outbound channel
// adapter the send path MUST surface a clean 404 instead of an empty
// 200 — ErrNotFound is the closest semantic match (the conversation
// the operator is trying to reply to does not exist yet on this
// listener).
type notFoundSendOutbound struct{}

func (notFoundSendOutbound) SendForView(_ context.Context, _ inboxusecase.SendOutboundInput) (inboxusecase.MessageView, error) {
	return inboxusecase.MessageView{}, inboxdomain.ErrNotFound
}

// notFoundGetMessage is the disabled-mode placeholder for the realtime
// status poll GET /inbox/conversations/{id}/messages/{msgID}/status.
// Same rationale as notFoundListMessages — no conversation, no message,
// 404 with no body.
type notFoundGetMessage struct{}

func (notFoundGetMessage) Execute(_ context.Context, _ inboxusecase.GetMessageInput) (inboxusecase.GetMessageResult, error) {
	return inboxusecase.GetMessageResult{}, inboxdomain.ErrNotFound
}
