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
//   - real        → returns nil + logs a clear "not yet wired" line.
//                   The route shell stays unmounted on this listener
//                   until SIN-63793 W3 ships the real-carrier wireup.
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
		return buildInboxHandlerDisabled()
	case InboxChannelProviderLLMCustomer:
		return buildInboxHandlerLLMCustomer(ctx, getenv)
	case InboxChannelProviderReal:
		log.Printf("crm: inbox handler disabled — provider %q is not yet wired (SIN-63793 W3 follow-up)", provider)
		return nil, noop
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
	noop := func() {}
	deps := webinbox.Deps{
		ListConversations: emptyListConversations{},
		ListMessages:      notFoundListMessages{},
		SendOutbound:      notFoundSendOutbound{},
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
		return nil, noop
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	log.Printf("crm: inbox HTMX routes mounted on public listener (provider=disabled, stub deps)")
	return mux, noop
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
