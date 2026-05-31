package main

// SIN-63821 (parent SIN-63793) — operator inbox HTMX wireup (W1).
//
// W1 mounts the /inbox/* route shell on the production router so
// `agent@<tenant>.crm.crm.someu.com.br` (RoleTenantAtendente) can reach
// the inbox surface today instead of bouncing off a 404. The channel
// adapter (W2), config selector (W4), and real wireup with non-nil deps
// (W5) land in follow-up issues — until then the handler runs with stub
// use cases that always return an empty list / not-found so the GET
// /inbox page renders the empty-inbox shell, and the conversation /
// send / status endpoints surface 404 / 400 cleanly.
//
// The handler.New constructor rejects nil required deps, so this wire
// supplies tiny in-process stubs rather than guarding the route mount
// on a nil dep — that keeps the chi route table stable across the W2-W5
// rollout (operators never see a regression from "404" → "200 empty" →
// "200 with data", just two state transitions).
//
// Returns (nil, no-op) so callers can `defer cleanup()` unconditionally;
// the wire never opens a pool today (the stub use cases need nothing),
// but the signature matches buildWebContactsHandler so W5 can swap
// the body without touching main.go.

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
// dispatch. cleanup is a no-op until W5 wires postgres-backed adapters.
func buildInboxHandler(_ context.Context, _ func(string) string) (http.Handler, func()) {
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
	log.Printf("crm: inbox HTMX routes mounted on public listener (W1 stub deps)")
	return mux, noop
}

// emptyListConversations is the W1 placeholder for the read-side that
// backs GET /inbox. Execute returns an empty Items slice for any tenant
// so the handler renders the empty-inbox shell (left list = empty, right
// pane = empty). W5 replaces this with the postgres-backed use case from
// internal/adapter/db/postgres/inbox.
type emptyListConversations struct{}

func (emptyListConversations) Execute(_ context.Context, _ inboxusecase.ListConversationsInput) (inboxusecase.ListConversationsResult, error) {
	return inboxusecase.ListConversationsResult{Items: nil}, nil
}

// notFoundListMessages is the W1 placeholder for GET
// /inbox/conversations/{id}. With no conversations seeded the handler
// MUST surface 404 on any direct visit; ErrNotFound is the documented
// signal the handler converts to http.StatusNotFound.
type notFoundListMessages struct{}

func (notFoundListMessages) Execute(_ context.Context, _ inboxusecase.ListMessagesInput) (inboxusecase.ListMessagesResult, error) {
	return inboxusecase.ListMessagesResult{}, inboxdomain.ErrNotFound
}

// notFoundSendOutbound is the W1 placeholder for POST
// /inbox/conversations/{id}/messages. Without an outbound channel
// adapter the send path MUST surface a clean 404 instead of an empty
// 200 — ErrNotFound is the closest semantic match (the conversation
// the operator is trying to reply to does not exist yet on this
// listener). W2 wires the LLM-customer channel that owns SendForView.
type notFoundSendOutbound struct{}

func (notFoundSendOutbound) SendForView(_ context.Context, _ inboxusecase.SendOutboundInput) (inboxusecase.MessageView, error) {
	return inboxusecase.MessageView{}, inboxdomain.ErrNotFound
}

// notFoundGetMessage is the W1 placeholder for the realtime status
// poll GET /inbox/conversations/{id}/messages/{msgID}/status. Same
// rationale as notFoundListMessages — no conversation, no message,
// 404 with no body.
type notFoundGetMessage struct{}

func (notFoundGetMessage) Execute(_ context.Context, _ inboxusecase.GetMessageInput) (inboxusecase.GetMessageResult, error) {
	return inboxusecase.GetMessageResult{}, inboxdomain.ErrNotFound
}
