package main

// SIN-62844 wiring — Messenger inbound webhook receiver + OutboundChannel
// sender for the Messenger channel (F2-10 follow-up, F2-05 media scan).
//
// Inbound path:
//   - Reuses the same META_APP_SECRET as WhatsApp (one Meta app per org).
//   - Resolves the Facebook Page ID from tenant_channel_associations
//     (channel="messenger") — same table WhatsApp uses for phone_number_id.
//   - EnvFeatureFlag (FEATURE_MESSENGER_ENABLED + FEATURE_MESSENGER_TENANTS).
//   - MediaScanPublisher is optional: when NATS is not wired the handler
//     still persists the placeholder body and logs a warn.
//
// Outbound path (OutboundChannel / SendMessage) — buildMessengerOutboundEntry:
//   - Resolves per-tenant PageID via a reverse lookup against
//     tenant_channel_associations with channel="messenger" (single-key pattern,
//     same as WhatsApp uses for phone_number_id in the same table).
//   - Token: META_MESSENGER_GRAPH_TOKEN if set, else falls back to
//     META_GRAPH_TOKEN (the WhatsApp system-user token). The two Meta
//     products are commonly authorized under the same Business Manager
//     system user (one token for both), but a Page can also live under a
//     different Business Manager / system user than the WhatsApp Business
//     Account — the dedicated override lets that Page's own token be used
//     for Messenger sends without touching WhatsApp's.
//   - Called exactly once, from inbox_wire_real.go's combined outbound
//     Router — NOT from assembleMessengerAdapter (inbound never needed it).
//     Building it twice would register the messenger_send_* Prometheus
//     collectors twice and panic at boot, the same class of bug fixed for
//     WhatsApp's outbound dispatcher (whatsapp_wire.go's dead duplicate
//     construction).
//
// Both paths gate on META_APP_SECRET and DATABASE_URL; any missing dep
// logs a "disabled" line and returns nil/false (fail-soft, same as
// whatsapp_wire).

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/adapter/channel/dispatch"
	channelmessenger "github.com/pericles-luz/crm/internal/adapter/channel/messenger"
	"github.com/pericles-luz/crm/internal/adapter/channels/messenger"
	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	rlredis "github.com/pericles-luz/crm/internal/adapter/ratelimit/redis"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// messengerWiring bundles the artifacts buildMessengerWiring produces.
type messengerWiring struct {
	Register func(*http.ServeMux)
	Cleanup  func()
}

// buildMessengerWiring assembles the production Messenger inbound adapter.
// Returns nil when any required env var is missing — the caller treats nil
// as "skip mounting Messenger routes". Outbound sending is built
// separately by buildMessengerOutboundEntry (see file doc comment).
func buildMessengerWiring(ctx context.Context, getenv func(string) string) *messengerWiring {
	cfg, err := messenger.ConfigFromEnv(getenv)
	if err != nil {
		log.Printf("crm: messenger intake disabled — %v", err)
		return nil
	}
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: messenger intake disabled (DATABASE_URL unset)")
		return nil
	}
	pool, err := pgpool.New(ctx, dsn)
	if err != nil {
		log.Printf("crm: messenger intake disabled — pg connect: %v", err)
		return nil
	}
	adapter, cleanup, err := assembleMessengerAdapter(ctx, cfg, pool, getenv)
	if err != nil {
		pool.Close()
		log.Printf("crm: messenger intake disabled — assemble: %v", err)
		return nil
	}
	register := func(mux *http.ServeMux) {
		adapter.Register(mux)
	}
	log.Printf("crm: messenger intake mounted on public listener")
	return &messengerWiring{Register: register, Cleanup: cleanup}
}

// assembleMessengerAdapter constructs the inbound adapter from
// already-connected dependencies. Split out so unit tests can wire fakes
// instead of real pgxpool clients.
func assembleMessengerAdapter(ctx context.Context, cfg messenger.Config, pool *pgxpool.Pool, getenv func(string) string) (*messenger.Adapter, func(), error) {
	contactsStore, err := pgcontacts.New(pool)
	if err != nil {
		return nil, nil, err
	}
	inboxStore, err := pginbox.New(pool)
	if err != nil {
		return nil, nil, err
	}
	contactsUC, err := contactsusecase.New(contactsStore)
	if err != nil {
		return nil, nil, err
	}
	receiver, err := inboxusecase.NewReceiveInbound(inboxStore, inboxStore, contactsUC)
	if err != nil {
		return nil, nil, err
	}
	// SIN-66378 P4 — route new conversations to the tenant channel
	// instance resolved from the inbound identity (channel_id). Soft-fail.
	wireChannelResolver(receiver, pool)
	// SIN-62959 — attribution hook. Same soft-fail pattern as
	// whatsapp_wire: linker construction failure → warn + skip; inbox
	// keeps working without the [crm:<click_id>] linkage.
	if linker, err := buildCampaignLinker(pool); err != nil {
		slog.Default().Warn("messenger wire: campaign linker disabled", "err", err)
	} else if linker != nil {
		receiver.SetCampaignLinker(linker)
		receiver.SetCampaignLinkerLogger(slog.Default())
		// SIN-62982 — share the marker signing key with the inbox
		// verifier so the redirect handler and the inbox-side hook
		// agree on the HMAC. Empty key keeps the compat-window
		// legacy-only behaviour.
		receiver.SetCampaignMarkerKey(readMarkerSigningKey(getenv))
	}
	// SIN-62960 — funnel engine fan-out hook. Mirrors whatsapp_wire:
	// NATS_URL unset → no publisher → inbox skips the publish; dial
	// error → warn + continue. The publish hook in ReceiveInbound is
	// soft-fail (see linkContactToCampaign for the same contract).
	funnelPub, funnelCleanup, err := buildFunnelEngineInboundPublisher(ctx, getenv)
	if err != nil {
		slog.Default().Warn("messenger wire: funnel engine publisher disabled", "err", err)
	} else if funnelPub != nil {
		receiver.SetInboundMessagePublisher(funnelPub)
		receiver.SetInboundMessagePublisherLogger(slog.Default())
	}

	lookup := pgstore.NewChannelAssociationLookup(pool)
	resolver := messenger.TenantResolverFunc(func(ctx context.Context, pageID string) (uuid.UUID, error) {
		id, err := lookup.Resolve(ctx, messenger.Channel, pageID)
		if errors.Is(err, pgstore.ErrAssociationUnknown) {
			return uuid.Nil, messenger.ErrUnknownPageID
		}
		return id, err
	})
	flag := messenger.NewEnvFeatureFlag(getenv)

	// message_deliveries / message_reads status parity with WhatsApp.
	// Both narrow read ports (contactsStore, inboxStore) already exist
	// in this function; MustNewUpdateMessageStatus is the same
	// channel-agnostic use case whatsapp_wire.go wires for WhatsApp's
	// statuses[] array.
	statusUpdater := inboxusecase.MustNewUpdateMessageStatus(inboxStore, inboxStore)

	inboundAdapter, err := messenger.New(cfg, receiver, resolver, flag,
		messenger.WithLogger(slog.Default()),
		messenger.WithStatusUpdater(statusUpdater),
		messenger.WithReadReceiptLookup(contactsStore, inboxStore),
		// MediaScanPublisher wired in a follow-up PR that lands the
		// NATS-backed publisher in cmd/server.
	)
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() {
		if funnelCleanup != nil {
			funnelCleanup()
		}
		pool.Close()
	}
	return inboundAdapter, cleanup, nil
}

// buildMessengerOutboundEntry builds the decorated (idempotent +
// rate-limited) Messenger sender WITHOUT wrapping it in a Router, so
// inbox_wire_real.go can merge it with the WhatsApp / fake-customer
// entries into one combined Router built once per boot. ok=false means
// Messenger outbound is disabled (no token / construction error) — the
// caller should omit the entry from any combined route map.
//
// This is the ONLY construction site for channelmessenger.Sender — do not
// also build one inside assembleMessengerAdapter (inbound doesn't need
// it); see the file doc comment for why a second construction site
// panics at boot.
func buildMessengerOutboundEntry(getenv func(string) string, pool *pgxpool.Pool, rdb *goredis.Client, flag *messenger.EnvFeatureFlag) (inbox.OutboundChannel, bool) {
	token := messengerOutboundGraphToken(getenv)
	if token == "" {
		log.Printf("crm: messenger outbound sender disabled (META_MESSENGER_GRAPH_TOKEN / META_GRAPH_TOKEN unset)")
		return nil, false
	}
	lookup := channelmessenger.TenantConfigLookup(func(ctx context.Context, tenantID uuid.UUID) (channelmessenger.TenantConfig, error) {
		pageID, err := messengerOutboundPageID(ctx, pool, tenantID)
		if err != nil {
			return channelmessenger.TenantConfig{}, err
		}
		on, flagErr := flag.Enabled(ctx, tenantID)
		if flagErr != nil {
			return channelmessenger.TenantConfig{}, flagErr
		}
		return channelmessenger.TenantConfig{PageID: pageID, Enabled: on}, nil
	})
	sender, err := channelmessenger.New(token, lookup, prometheus.DefaultRegisterer)
	if err != nil {
		log.Printf("crm: messenger outbound sender disabled — %v", err)
		return nil, false
	}
	var limiter dispatch.RateLimiter
	if rdb != nil {
		limiter = rlredis.New(rdb, "messenger:out:")
	}
	log.Printf("crm: messenger outbound sender ready")
	var oc inbox.OutboundChannel = sender
	oc = dispatch.NewRateLimited(oc, limiter, time.Minute, messengerOutboundRateMaxPerMin(getenv), nil)
	oc = dispatch.NewIdempotent(oc, dispatch.NewMemoryLedger())
	return oc, true
}

// envMessengerGraphToken optionally overrides META_GRAPH_TOKEN for
// Messenger sends. Useful when the Facebook Page is authorized under a
// different Business Manager / system user than the WhatsApp Business
// Account (so a single META_GRAPH_TOKEN can't cover both products).
const envMessengerGraphToken = "META_MESSENGER_GRAPH_TOKEN"

// messengerOutboundGraphToken reads META_MESSENGER_GRAPH_TOKEN, falling
// back to the shared META_GRAPH_TOKEN (the common case: one Business
// Manager system user authorized for both WhatsApp and Messenger).
func messengerOutboundGraphToken(getenv func(string) string) string {
	if token := strings.TrimSpace(getenv(envMessengerGraphToken)); token != "" {
		return token
	}
	return getenv("META_GRAPH_TOKEN")
}

// defaultMessengerRateMaxPerMin mirrors defaultOutboundRateMaxPerMin
// (outbound_dispatch_wire.go) for the Messenger channel.
const defaultMessengerRateMaxPerMin = 600

// messengerOutboundRateMaxPerMin reads MESSENGER_RATE_MAX_PER_MIN, falling
// back to defaultMessengerRateMaxPerMin for an unset or non-positive value.
func messengerOutboundRateMaxPerMin(getenv func(string) string) int {
	raw := strings.TrimSpace(getenv("MESSENGER_RATE_MAX_PER_MIN"))
	if raw == "" {
		return defaultMessengerRateMaxPerMin
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultMessengerRateMaxPerMin
	}
	return n
}

// messengerOutboundPageID returns the Facebook Page ID associated with a
// tenant for outbound sends. It queries tenant_channel_associations with
// channel="messenger" and tenant_id=$1 (reverse of the inbound direction).
//
// When no row exists the tenant has not configured a Messenger Page; the
// caller receives ErrChannelAuthFailed (via messengerOutboundPageID →
// TenantConfig.PageID=="").
func messengerOutboundPageID(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID) (string, error) {
	const sql = `
		SELECT association
		  FROM tenant_channel_associations
		 WHERE channel = $1 AND tenant_id = $2
		 LIMIT 1`
	var pageID string
	err := pool.QueryRow(ctx, sql, messenger.Channel, tenantID).Scan(&pageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil // caller surfaces as ErrChannelAuthFailed via empty PageID
	}
	if err != nil {
		return "", err
	}
	return pageID, nil
}
