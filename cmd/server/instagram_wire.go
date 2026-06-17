package main

// SIN-64971 wiring — Instagram Direct inbound webhook receiver.
//
// Verification (issue Passo 1): the internal/adapter/channels/instagram
// package was complete and integration-tested (HMAC → dedup →
// ReceiveInbound → Postgres) but was never wired into the composition
// root. In production inbound Instagram messages only reached the
// generic Meta webhook adapter (webhook_wire.go), which stores a raw
// event and never calls inboxusecase.ReceiveInbound — so no contact,
// conversation, or message was persisted and nothing appeared in
// /inbox. This wire closes that gap by mounting the dedicated adapter
// on the same use case WhatsApp and Messenger use.
//
// The wire-up gates on three required env vars (META_APP_SECRET,
// META_INSTAGRAM_VERIFY_TOKEN, DATABASE_URL) plus REDIS_URL for the
// per-ig_business_id rate limiter. Any missing dep logs a "disabled"
// line and returns nil so the public listener boots without the
// Instagram routes registered — same fail-soft pattern as
// whatsapp_wire / messenger_wire.
//
// Identity + idempotency come for free by reusing the exact stack the
// WhatsApp and Messenger wires assemble:
//   - pgcontacts.New + contactsusecase.New → contact upsert keyed on
//     (channel, external_id) in contact_channel_identity (ADR 0020).
//   - inboxusecase.NewReceiveInbound → Claim → SaveMessage →
//     MarkProcessed inside one transaction; concurrent replays of the
//     same mid bounce on the inbound_message_dedup UNIQUE constraint
//     and return a silent 200 without a second Message row.

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	rlredis "github.com/pericles-luz/crm/internal/adapter/ratelimit/redis"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// instagramWiring bundles the artifacts buildInstagramWiring produces.
// Register adds POST + GET /webhooks/instagram onto a stdlib mux;
// Cleanup releases the pool/Redis client when the listener shuts down.
type instagramWiring struct {
	Register func(*http.ServeMux)
	Cleanup  func()
}

// buildInstagramWiring assembles the production Instagram inbound
// adapter. Returns nil when any required env var is missing — the
// caller treats nil as "skip mounting Instagram routes".
func buildInstagramWiring(ctx context.Context, getenv func(string) string) *instagramWiring {
	cfg, err := instagram.ConfigFromEnv(getenv)
	if err != nil {
		log.Printf("crm: instagram intake disabled — %v", err)
		return nil
	}
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: instagram intake disabled (DATABASE_URL unset)")
		return nil
	}
	redisURL := getenv(envRedisURL)
	if redisURL == "" {
		log.Printf("crm: instagram intake disabled (REDIS_URL unset)")
		return nil
	}
	pool, err := pgpool.New(ctx, dsn)
	if err != nil {
		log.Printf("crm: instagram intake disabled — pg connect: %v", err)
		return nil
	}
	rdb, err := newRedisClient(redisURL)
	if err != nil {
		pool.Close()
		log.Printf("crm: instagram intake disabled — redis connect: %v", err)
		return nil
	}
	adapter, cleanup, err := assembleInstagramAdapter(ctx, cfg, pool, rdb, getenv)
	if err != nil {
		pool.Close()
		_ = rdb.Close()
		log.Printf("crm: instagram intake disabled — assemble: %v", err)
		return nil
	}
	register := func(mux *http.ServeMux) {
		adapter.Register(mux)
	}
	log.Printf("crm: instagram intake mounted on public listener")
	return &instagramWiring{Register: register, Cleanup: cleanup}
}

// assembleInstagramAdapter constructs the adapter from already-connected
// dependencies. Split out so unit tests can wire fakes instead of real
// pgxpool / redis clients (mirrors assembleWhatsAppAdapter).
func assembleInstagramAdapter(ctx context.Context, cfg instagram.Config, pool *pgxpool.Pool, rdb *goredis.Client, getenv func(string) string) (*instagram.Adapter, func(), error) {
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
	// SIN-62959 — attribution hook. Same soft-fail pattern as
	// whatsapp_wire / messenger_wire: linker construction failure →
	// warn + skip; the inbox keeps working without the [crm:<click_id>]
	// linkage.
	if linker, err := buildCampaignLinker(pool); err != nil {
		slog.Default().Warn("instagram wire: campaign linker disabled", "err", err)
	} else if linker != nil {
		receiver.SetCampaignLinker(linker)
		receiver.SetCampaignLinkerLogger(slog.Default())
		// SIN-62982 — share the marker signing key with the inbox
		// verifier so the redirect handler and the inbox-side hook
		// agree on the HMAC. Empty key keeps the compat-window
		// legacy-only behaviour.
		receiver.SetCampaignMarkerKey(readMarkerSigningKey(getenv))
	}
	// SIN-62960 — funnel engine fan-out hook. NATS_URL unset → no
	// publisher → inbox skips the publish; dial error → warn + continue.
	funnelPub, funnelCleanup, err := buildFunnelEngineInboundPublisher(ctx, getenv)
	if err != nil {
		slog.Default().Warn("instagram wire: funnel engine publisher disabled", "err", err)
	} else if funnelPub != nil {
		receiver.SetInboundMessagePublisher(funnelPub)
		receiver.SetInboundMessagePublisherLogger(slog.Default())
	}

	lookup := pgstore.NewChannelAssociationLookup(pool)
	resolver := instagram.TenantResolverFunc(func(ctx context.Context, igBusinessID string) (uuid.UUID, error) {
		id, err := lookup.Resolve(ctx, instagram.Channel, igBusinessID)
		if errors.Is(err, pgstore.ErrAssociationUnknown) {
			return uuid.Nil, instagram.ErrUnknownIGBusinessID
		}
		return id, err
	})
	rl := rlredis.New(rdb, "instagram")
	flag := instagram.NewEnvFeatureFlag(getenv)

	adapter, err := instagram.New(cfg, receiver, resolver, flag, rl,
		instagram.WithLogger(slog.Default()),
		// MediaScanPublisher wired in a follow-up PR that lands the
		// NATS-backed publisher in cmd/server (mirrors messenger_wire).
	)
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() {
		if funnelCleanup != nil {
			funnelCleanup()
		}
		pool.Close()
		_ = rdb.Close()
	}
	return adapter, cleanup, nil
}
