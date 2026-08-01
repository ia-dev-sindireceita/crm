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
//   - Outbound sends go to one combined dispatch.Router keyed by channel,
//     merging the WhatsApp Cloud entry (outbound_dispatch_wire.go,
//     SIN-68306) with the Messenger entry (messenger_wire.go). A channel
//     with no entry fails closed with ErrChannelDisabled (Router's
//     deny-by-default), so an operator reply on an unconfigured channel
//     cleanly fails closed instead of dispatching to a live carrier.
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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/adapter/channel/dispatch"
	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
	"github.com/pericles-luz/crm/internal/adapter/channels/messenger"
	"github.com/pericles-luz/crm/internal/adapter/channels/whatsapp"
	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	"github.com/pericles-luz/crm/internal/contacts"
	"github.com/pericles-luz/crm/internal/inbox"
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

	// Outbound router (SIN-68306 + Messenger wiring). One entry per
	// channel; a channel with no entry fails closed with
	// ErrChannelDisabled (dispatch.Router's deny-by-default).
	routes := map[string]inbox.OutboundChannel{}

	// WhatsApp entry: real routed Sender when META_GRAPH_TOKEN is
	// present, absent otherwise. The feature flag re-checks the
	// per-tenant allowlist inside the dispatcher's TenantConfig lookup.
	flag := whatsapp.NewEnvFeatureFlag(getenv)
	if entry, ok := buildWhatsAppOutboundEntry(getenv, pool, rdb, flag); ok {
		routes[whatsapp.Channel] = entry
	}

	// Messenger entry: same shape as WhatsApp — real routed Sender when
	// META_MESSENGER_GRAPH_TOKEN/META_GRAPH_TOKEN is present, absent
	// otherwise. buildMessengerOutboundEntry (messenger_wire.go) is the
	// ONLY construction site for the Messenger sender — do not also
	// build one in messenger_wire.go's inbound assembly (see that file's
	// doc comment for the duplicate-Prometheus-registration panic this
	// would otherwise cause).
	msgFlag := messenger.NewEnvFeatureFlag(getenv)
	if entry, ok := buildMessengerOutboundEntry(getenv, pool, rdb, msgFlag); ok {
		routes[messenger.Channel] = entry
	}

	// Instagram entry: same shape as WhatsApp/Messenger — real routed
	// Sender when META_INSTAGRAM_GRAPH_TOKEN/META_GRAPH_TOKEN is present,
	// absent otherwise. buildInstagramOutboundEntry (instagram_outbound_wire.go)
	// is the ONLY construction site for the Instagram sender — do not
	// also build one in instagram_wire.go's inbound assembly (duplicate-
	// Prometheus-registration panic, see that file's doc comment).
	igFlag := instagram.NewEnvFeatureFlag(getenv)
	if entry, ok := buildInstagramOutboundEntry(getenv, pool, rdb, igFlag); ok {
		routes[instagram.Channel] = entry
	}

	outbound := dispatch.NewRouter(routes)

	// SendOutbound resolves the recipient's identity from the
	// conversation's channel + contact (the web handler leaves
	// ToExternalID empty): WhatsApp resolves E.164, Messenger and
	// Instagram resolve PSID/IGSID. passthroughWalletDebitor keeps the
	// reserve→charge→commit ordering with a zero cost until the tariff
	// wallet adapter lands (a separate slice).
	sendUC, err := inboxusecase.NewSendOutbound(
		inboxStore,
		passthroughWalletDebitor{},
		outbound,
		inboxusecase.WithContactLookup(combinedOutboundContactLookup(inboxStore, contactsStore)),
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

// combinedOutboundContactLookup resolves a conversation to the
// recipient's channel-side identity: WhatsApp, Messenger, and Instagram
// fall through to the matching contact identity (E.164 / PSID / IGSID
// respectively) — whatsapp_outbound_wire.go's whatsappOutboundContactLookup
// inlined here since every branch needs the same conversation read first.
func combinedOutboundContactLookup(convs conversationResolver, finder contactIdentityFinder) inboxusecase.ContactLookupFn {
	return func(ctx context.Context, tenantID, conversationID uuid.UUID) (string, error) {
		conv, err := convs.GetConversation(ctx, tenantID, conversationID)
		if err != nil {
			return "", err
		}
		identityChannel := contacts.ChannelWhatsApp
		if conv.Channel == messenger.Channel {
			identityChannel = contacts.ChannelMessenger
		} else if conv.Channel == instagram.Channel {
			identityChannel = contacts.ChannelInstagram
		}
		c, err := finder.FindByID(ctx, tenantID, conv.ContactID)
		if err != nil {
			return "", err
		}
		for _, id := range c.Identities() {
			if id.Channel == identityChannel {
				return id.ExternalID, nil
			}
		}
		return "", fmt.Errorf("outbound: contact %s has no %s identity", conv.ContactID, identityChannel)
	}
}
