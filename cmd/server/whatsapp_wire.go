package main

// SIN-62731 wiring — WhatsApp Cloud-API inbound webhook receiver.
//
// The wire-up gates on three required env vars (META_APP_SECRET,
// META_VERIFY_TOKEN, DATABASE_URL) plus the global FEATURE_WHATSAPP_ENABLED
// flag. Any missing dep logs a "disabled" line and returns nil so the
// public listener boots without the WhatsApp routes registered — same
// fail-soft pattern as customdomain_wire and webhook_wire.
//
// Production wiring:
//   - Postgres pool for inbox + contacts + tenant_channel_associations
//   - Redis sliding-window rate limiter keyed on phone_number_id
//   - EnvFeatureFlag (FEATURE_WHATSAPP_ENABLED + FEATURE_WHATSAPP_TENANTS)
//
// The two-layer idempotency contract from ADR 0087 §D3 holds: the
// inbound_message_dedup row is INSERTed inside ReceiveInbound's
// transaction (Claim → SaveMessage → MarkProcessed). Concurrent
// replays of the same wamid bounce on the UNIQUE constraint and
// return nil to the HTTP layer (silent 200) without producing a
// second Message row.

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/adapter/channels/whatsapp"
	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	rlredis "github.com/pericles-luz/crm/internal/adapter/ratelimit/redis"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// whatsappWiring bundles the artifacts buildWhatsAppWiring produces.
// Register adds POST + GET /webhooks/whatsapp onto a stdlib mux;
// Cleanup releases the pool/Redis client when the listener shuts down.
type whatsappWiring struct {
	Register func(*http.ServeMux)
	Cleanup  func()
}

// buildWhatsAppWiring assembles the production WhatsApp inbound adapter.
// Returns nil when any required env var is missing — the caller treats
// nil as "skip mounting WhatsApp routes".
func buildWhatsAppWiring(ctx context.Context, getenv func(string) string) *whatsappWiring {
	cfg, err := whatsapp.ConfigFromEnv(getenv)
	if err != nil {
		log.Printf("crm: whatsapp intake disabled — %v", err)
		return nil
	}
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: whatsapp intake disabled (DATABASE_URL unset)")
		return nil
	}
	redisURL := getenv(envRedisURL)
	if redisURL == "" {
		log.Printf("crm: whatsapp intake disabled (REDIS_URL unset)")
		return nil
	}
	pool, err := pgpool.New(ctx, dsn)
	if err != nil {
		log.Printf("crm: whatsapp intake disabled — pg connect: %v", err)
		return nil
	}
	rdb, err := newRedisClient(redisURL)
	if err != nil {
		pool.Close()
		log.Printf("crm: whatsapp intake disabled — redis connect: %v", err)
		return nil
	}
	adapter, cleanup, err := assembleWhatsAppAdapter(cfg, pool, rdb, getenv)
	if err != nil {
		pool.Close()
		_ = rdb.Close()
		log.Printf("crm: whatsapp intake disabled — assemble: %v", err)
		return nil
	}
	register := func(mux *http.ServeMux) {
		adapter.Register(mux)
	}
	log.Printf("crm: whatsapp intake mounted on public listener")
	return &whatsappWiring{Register: register, Cleanup: cleanup}
}

// assembleWhatsAppAdapter constructs the adapter from already-connected
// dependencies. Split out so unit tests can wire fakes instead of real
// pgxpool / redis clients.
func assembleWhatsAppAdapter(cfg whatsapp.Config, pool *pgxpool.Pool, rdb *goredis.Client, getenv func(string) string) (*whatsapp.Adapter, func(), error) {
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
	lookup := pgstore.NewChannelAssociationLookup(pool)
	resolver := whatsapp.TenantResolverFunc(func(ctx context.Context, pn string) (uuid.UUID, error) {
		id, err := lookup.Resolve(ctx, whatsapp.Channel, pn)
		if errors.Is(err, pgstore.ErrAssociationUnknown) {
			return uuid.Nil, whatsapp.ErrUnknownPhoneNumberID
		}
		return id, err
	})
	rl := rlredis.New(rdb, "whatsapp")
	flag := whatsapp.NewEnvFeatureFlag(getenv)
	adapter, err := whatsapp.New(cfg, receiver, resolver, flag, rl,
		whatsapp.WithLogger(slog.Default()),
		// SIN-62762: register the inbound-handler latency histogram
		// (whatsapp_handler_elapsed_seconds) on the global registry so
		// scrape endpoints under /metrics include it. Runbook:
		// docs/runbooks/whatsapp-inbound-latency.md.
		whatsapp.WithMetricsRegistry(prometheus.DefaultRegisterer))
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		pool.Close()
		_ = rdb.Close()
	}
	return adapter, cleanup, nil
}

// newRedisClient is the Redis dial helper. Production calls this from
// buildWhatsAppWiring; unit tests stub the assembler instead.
func newRedisClient(url string) (*goredis.Client, error) {
	opt, err := goredis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	rdb := goredis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	return rdb, nil
}
