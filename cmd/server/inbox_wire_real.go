package main

// SIN-67470 / SIN-63793 W3 — real-carrier branch of buildInboxHandler.
//
// This is the production read+write inbox for a live carrier. Inbound
// WhatsApp messages are received and persisted by whatsapp_wire.go
// (POST /webhooks/whatsapp → ReceiveInbound → Postgres); this wire mounts
// the operator-facing /inbox surface on top of that same Postgres storage
// so the persisted conversations and messages become visible in the UI.
//
// Before this wire the real provider returned a nil handler (the W3
// "not yet wired" stub in inbox_wire.go), so /inbox 404'd and inbound
// messages sat invisibly in Postgres. This closes that gap.
//
// Difference from the llmcustomer branch (inbox_wire_llmcustomer.go):
//   - No fake channel adapter. There is no synthetic-customer Bootstrap
//     and no auto-reply loop — conversations arrive only from the real
//     carrier webhook, so the read use cases front the postgres store
//     directly with no bootstrap decorator.
//   - Outbound sends go to the real WhatsApp Cloud dispatcher built by
//     buildWhatsAppOutbound (outbound_dispatch_wire.go, SIN-68306): a
//     channel-routed, idempotent, rate-limited Sender. Deny-by-default —
//     when META_GRAPH_TOKEN is unset the dispatcher is an empty Router
//     that answers every send with ErrChannelDisabled, so an operator
//     reply cleanly fails closed instead of dispatching to a live carrier.
//
// Fail-soft (identical posture to every other cmd/server wire): DATABASE_URL
// unset OR any postgres construction error reverts to the disabled-mode
// stubs so the listener stays bootable and the /inbox route table stays
// stable (operators never see a 404 → 200 regression). REDIS_URL is
// optional here: it only backs the outbound per-tenant rate limiter, so a
// missing/unreachable Redis degrades to an unlimited (still token-gated)
// dispatcher rather than downing the inbox.
//
// Security (secure-by-default): META_GRAPH_TOKEN / META_APP_SECRET are read
// only via getenv, live only inside the Sender value, and are never logged
// or placed in a URL. This wire logs tenant-agnostic readiness lines only.

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/adapter/channels/whatsapp"
	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// buildInboxHandlerReal is the production wrapper for the real-carrier
// inbox. It opens DATABASE_URL (required) and REDIS_URL (optional, rate
// limiter only), builds the pg-backed inbox + contacts + user-directory
// stores, wires the WhatsApp outbound dispatcher, and delegates the
// assembly to assembleInboxHandlerRealFromPool. On any failure (missing
// DSN, connect error, assembly error) it falls back to the disabled-mode
// stubs so the route shell stays mounted and boot stays soft-fail —
// consistent with buildInboxHandlerLLMCustomer and the other web/* wires.
func buildInboxHandlerReal(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: inbox handler degraded — provider=real but DATABASE_URL unset; falling back to disabled stubs")
		return buildInboxHandlerDisabled()
	}
	pool, err := pgpool.New(ctx, dsn)
	if err != nil {
		log.Printf("crm: inbox handler degraded — provider=real pg connect: %v; falling back to disabled stubs", err)
		return buildInboxHandlerDisabled()
	}

	// Redis backs only the outbound rate limiter (buildWhatsAppOutbound).
	// It is optional: an unset or unreachable REDIS_URL degrades the
	// dispatcher to no rate limiting (still deny-by-default on the Graph
	// token) rather than failing the inbox mount.
	var rdb *goredis.Client
	if redisURL := getenv(envRedisURL); redisURL != "" {
		client, rerr := newRedisClient(redisURL)
		if rerr != nil {
			log.Printf("crm: inbox handler (real) — redis connect: %v; outbound rate limiting disabled", rerr)
		} else {
			rdb = client
		}
	}

	mux, cleanup, err := assembleInboxHandlerRealFromPool(pool, rdb, getenv)
	if err != nil {
		if rdb != nil {
			_ = rdb.Close()
		}
		pool.Close()
		log.Printf("crm: inbox handler degraded — provider=real assemble: %v; falling back to disabled stubs", err)
		return buildInboxHandlerDisabled()
	}

	log.Printf("crm: inbox HTMX routes mounted on public listener (provider=real, WhatsApp carrier wired)")
	wrappedCleanup := func() {
		cleanup()
		if rdb != nil {
			_ = rdb.Close()
		}
		pool.Close()
	}
	return mux, wrappedCleanup
}

// assembleInboxHandlerRealFromPool wires the postgres-backed inbox read
// and write use cases onto the real WhatsApp outbound dispatcher and
// returns the stdlib *http.ServeMux webinbox.Handler.Routes produces plus
// a cleanup closure. The pool + redis client lifecycles are owned by the
// caller (buildInboxHandlerReal), so the returned cleanup is a no-op today
// — split out so a future test can assemble from an injected pool without
// paying for the production "open DATABASE_URL" step.
//
// rdb may be nil (Redis optional): buildWhatsAppOutbound treats a nil
// client as "no rate limiter" and still returns a usable dispatcher.
func assembleInboxHandlerRealFromPool(pool *pgxpool.Pool, rdb *goredis.Client, getenv func(string) string) (http.Handler, func(), error) {
	inboxStore, err := pginbox.New(pool)
	if err != nil {
		return nil, nil, fmt.Errorf("pginbox.New: %w", err)
	}
	contactsStore, err := pgcontacts.New(pool)
	if err != nil {
		return nil, nil, fmt.Errorf("pgcontacts.New: %w", err)
	}
	// The same UserDirectory adapter resolves the assigned-atendente chip
	// on the enriched list AND the top-bar account label, so both agree.
	userDir, err := pginbox.NewUserDirectory(pool)
	if err != nil {
		return nil, nil, fmt.Errorf("pginbox.NewUserDirectory: %w", err)
	}

	// Outbound dispatcher (SIN-68306). Always non-nil: a real routed
	// Sender when META_GRAPH_TOKEN is present, an empty no-op Router
	// otherwise (deny-by-default). The feature flag re-checks the
	// per-tenant allowlist inside the dispatcher's TenantConfig lookup.
	flag := whatsapp.NewEnvFeatureFlag(getenv)
	outbound := buildWhatsAppOutbound(getenv, pool, rdb, flag)

	// SendOutbound resolves the recipient's WhatsApp E.164 from the
	// conversation's contact (the web handler leaves ToExternalID empty),
	// mirroring the disabled-provider outbound path in
	// whatsapp_outbound_wire.go. passthroughWalletDebitor keeps the
	// reserve→charge→commit ordering with a zero cost until the tariff
	// wallet adapter lands (a separate slice).
	sendUC, err := inboxusecase.NewSendOutbound(
		inboxStore,
		passthroughWalletDebitor{},
		outbound,
		inboxusecase.WithContactLookup(whatsappOutboundContactLookup(inboxStore, contactsStore)),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("send outbound usecase: %w", err)
	}

	// Conversation-context read feeds the channel + contact + assignment
	// to the customer panel. Funnel readers are nil: this wire mounts no
	// funnel storage, so the stage fields degrade to zero-values.
	ctxUC, err := inboxusecase.NewGetConversationContext(inboxStore, contactsStore, nil, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("conversation context usecase: %w", err)
	}

	// Assignment write path + dropdown (SIN-64979). *pginbox.Store
	// satisfies the reader, lead ledger, lead cache, and attendant-gate
	// ports, so the single store instance backs all four with no
	// consistency gap.
	assignUC := inboxusecase.MustNewAssignConversation(inboxStore, inboxStore, inboxStore, inboxStore)
	listAssignableUC := &listAssignableAdapter{r: inboxStore}

	deps := webinbox.Deps{
		ListConversations: inboxusecase.MustNewListConversations(inboxStore),
		// *pginbox.Store satisfies inbox.ConversationReadModel, so the same
		// store backs the enriched GET /inbox list (snippet + atendente +
		// filters) that surfaces persisted inbound messages (SIN-64968).
		ListSummaries:       inboxusecase.MustNewListConversationSummaries(inboxStore, userDir),
		ListMessages:        inboxusecase.MustNewListMessages(inboxStore),
		ListMessagesSince:   inboxusecase.MustNewListMessagesSince(inboxStore),
		SendOutbound:        sendUC,
		GetMessage:          inboxusecase.MustNewGetMessage(inboxStore),
		ConversationContext: ctxUC,
		AssignConversation:  assignUC,
		ListAssignable:      listAssignableUC,
		// SIN-66378 P4 — per-channel access scope on the live read path.
		// Soft-degrade: a build fault disables the filter + chip (nil) but
		// never downs the inbox; IsGerente reads the request principal.
		ChannelScope: buildInboxChannelScope(pool),
		IsGerente:    isGerenteFromSessionContext,
		CSRFToken:    csrfTokenFromSessionContext,
		UserID:       userIDFromSessionContext,
		UserLabels:   userDir,
		Logger:       slog.Default(),
	}

	h, err := webinbox.New(deps)
	if err != nil {
		return nil, nil, fmt.Errorf("webinbox.New: %w", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux, func() {}, nil
}
