package main

// SIN-64972 wiring — Webchat public widget surface (ADR-0021).
//
// The webchat adapter (internal/adapter/channels/webchat) and its
// session migration (0096) shipped, but no cmd/server wire mounted the
// routes, so the visitor inbound never reached the inbox. This wire
// assembles the production adapter and returns the http.Handler that
// httpapi.NewRouter mounts under the tenant-scoped (NOT authenticated)
// chi group, alongside GET /c/{slug} and GET /privacy:
//
//	POST /widget/v1/session   anonymous session + CSRF token (D2/D3/D4)
//	POST /widget/v1/message   visitor message, CSRF-validated (D3/D5/D7)
//	GET  /widget/v1/stream    SSE of operator replies (D5)
//
// Identity + idempotency come for free by reusing the exact inbox stack
// WhatsApp / Messenger / Instagram assemble:
//   - pgcontacts.New + contactsusecase.New → contact upsert keyed on
//     (channel, external_id) and identity merge (ADR 0020 D6 path 1).
//   - inboxusecase.NewReceiveInbound → Claim → SaveMessage →
//     MarkProcessed in one transaction; concurrent replays of the same
//     (session_id, client_msg_id) bounce on inbound_message_dedup and
//     return a silent 204 without a second Message row (ADR-0021 D7).
//
// The handler is mounted unconditionally when the pool is present; the
// per-tenant feature flag (FEATURE_WEBCHAT_ENABLED, default OFF) gates
// every request to 404 until a tenant is allow-listed (ADR-0021 D7), so
// a wired-but-disabled channel never leaks tenant existence.

import (
	"log"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	webchat "github.com/pericles-luz/crm/internal/adapter/channels/webchat"
	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// buildWebchatHandler assembles the webchat adapter on the supplied IAM
// pool (no second pgxpool opened) and returns the http.Handler for the
// three /widget/v1 routes. A nil pool returns (nil, nil) so partial-
// stack boots simply omit the routes. A construction failure returns a
// non-nil error; the caller logs it and omits the routes (non-fatal).
func buildWebchatHandler(pool *pgxpool.Pool, getenv func(string) string) (http.Handler, error) {
	if pool == nil {
		return nil, nil
	}

	contactsStore, err := pgcontacts.New(pool)
	if err != nil {
		return nil, err
	}
	inboxStore, err := pginbox.New(pool)
	if err != nil {
		return nil, err
	}
	contactsUC, err := contactsusecase.New(contactsStore)
	if err != nil {
		return nil, err
	}
	receiver, err := inboxusecase.NewReceiveInbound(inboxStore, inboxStore, contactsUC)
	if err != nil {
		return nil, err
	}

	adapter, err := webchat.New(
		receiver,
		postgresadapter.NewWebchatSessionStore(pool),
		postgresadapter.NewWebchatOrigins(pool),
		webchat.NewEnvFeatureFlag(getenv),
		webchat.NewWindowRateLimiter(),
		webchat.NewBroker(),
		nil, // ContactSignalUpdater (D6 part 2) wired in a follow-up.
	)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	adapter.Register(mux)
	log.Printf("crm: webchat widget routes mounted on tenant-scoped public group")
	return mux, nil
}
