package main

// SIN-62964 — wiring for POST /webhooks/pix/inter.
//
// Env-gated, fail-soft just like the other Phase-4 wires: a missing
// DATABASE_URL / REDIS_URL / PIX_INTER_WEBHOOK_SECRET collapses the
// wiring to nil and the route stays unmounted. cmd/server boots
// cleanly in degraded modes without the PIX webhook surface — the
// dev-loop and the migration-only smoke runs never need it.
//
// Required env to enable:
//
//   PIX_INTER_WEBHOOK_ENABLED=1
//   DATABASE_URL=...                  (master_ops pool dial)
//   REDIS_URL=...                     (rate-limit sliding window)
//   PIX_INTER_WEBHOOK_SECRET=...      (HMAC shared secret)
//   PIX_INTER_WEBHOOK_ACTOR_ID=<uuid> (master_ops audit actor)
//
// Optional:
//
//   PIX_INTER_WEBHOOK_IP_ALLOWLIST=10.0.0.0/8,192.168.1.0/24
//                                     (overrides DefaultAllowedCIDRs)
//   PIX_INTER_WEBHOOK_IP_CHECK=0      ("0"/"false" temporarily disables
//                                      the allowlist for troubleshoot;
//                                      WARN is logged at boot)
//   PIX_INTER_WEBHOOK_SIGNATURE_HEADER=X-Inter-Signature
//                                     (override header name)
//   PIX_INTER_WEBHOOK_IP_RATE_PER_MIN=100
//   PIX_INTER_WEBHOOK_EXT_RATE_PER_MIN=5
//
// Production deploys MUST set the secret + actor; any other missing
// required env keeps the route disabled with a log line naming the
// gap.

import (
	"context"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgpix "github.com/pericles-luz/crm/internal/adapter/db/postgres/pix"
	pixinter "github.com/pericles-luz/crm/internal/adapter/pix/inter"
	rlredis "github.com/pericles-luz/crm/internal/adapter/ratelimit/redis"
	httppix "github.com/pericles-luz/crm/internal/adapter/transport/http/pix"
	domainpix "github.com/pericles-luz/crm/internal/billing/pix"
)

const (
	envPixInterWebhookEnabled   = "PIX_INTER_WEBHOOK_ENABLED"
	envPixInterWebhookSecret    = "PIX_INTER_WEBHOOK_SECRET"
	envPixInterWebhookActor     = "PIX_INTER_WEBHOOK_ACTOR_ID"
	envPixInterWebhookAllowlist = "PIX_INTER_WEBHOOK_IP_ALLOWLIST"
	envPixInterWebhookIPCheck   = "PIX_INTER_WEBHOOK_IP_CHECK"
	envPixInterWebhookSigHeader = "PIX_INTER_WEBHOOK_SIGNATURE_HEADER"
	envPixInterWebhookIPRate    = "PIX_INTER_WEBHOOK_IP_RATE_PER_MIN"
	envPixInterWebhookExtRate   = "PIX_INTER_WEBHOOK_EXT_RATE_PER_MIN"
	pixInterRateRedisPrefix     = "pix-inter:rl:"
)

// pixInterWebhookWiring bundles the artifacts run()/runWith() needs to
// mount the receiver.
type pixInterWebhookWiring struct {
	Register func(*http.ServeMux)
	Cleanup  func()
}

// pixInterWebhookPool is the small pgxpool surface the wiring needs.
// *pgxpool.Pool satisfies it.
type pixInterWebhookPool interface {
	pgpool.TxBeginner
	Close()
}

// pixInterWebhookDial is the test seam. Production opens a real
// pgxpool; tests inject a fake.
type pixInterWebhookDial func(ctx context.Context, dsn string) (pixInterWebhookPool, error)

func defaultPixInterWebhookDial(ctx context.Context, dsn string) (pixInterWebhookPool, error) {
	pool, err := pgpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return pool, nil
}

// pixInterWebhookRedisDial is the seam for the rate-limiter client.
// Production uses goredis.ParseURL + goredis.NewClient; tests inject a
// stub.
type pixInterWebhookRedisDial func(redisURL string) (*goredis.Client, error)

func defaultPixInterWebhookRedisDial(redisURL string) (*goredis.Client, error) {
	opt, err := goredis.ParseURL(redisURL)
	if err != nil {
		return nil, err
	}
	return goredis.NewClient(opt), nil
}

// buildPixInterWebhookWiring is the production entry point.
func buildPixInterWebhookWiring(ctx context.Context, getenv func(string) string) *pixInterWebhookWiring {
	return buildPixInterWebhookWiringWithDeps(ctx, getenv, defaultPixInterWebhookDial, defaultPixInterWebhookRedisDial)
}

// buildPixInterWebhookWiringWithDeps is the test seam — accepts
// injectable dialers so unit tests can drive the wiring without
// Postgres / Redis.
func buildPixInterWebhookWiringWithDeps(
	ctx context.Context,
	getenv func(string) string,
	dial pixInterWebhookDial,
	redisDial pixInterWebhookRedisDial,
) *pixInterWebhookWiring {
	if getenv(envPixInterWebhookEnabled) != "1" {
		return nil
	}
	secret := getenv(envPixInterWebhookSecret)
	if secret == "" {
		log.Printf("crm: pix.inter webhook disabled (%s unset)", envPixInterWebhookSecret)
		return nil
	}
	actorRaw := strings.TrimSpace(getenv(envPixInterWebhookActor))
	if actorRaw == "" {
		log.Printf("crm: pix.inter webhook disabled (%s unset)", envPixInterWebhookActor)
		return nil
	}
	actor, err := uuid.Parse(actorRaw)
	if err != nil {
		log.Printf("crm: pix.inter webhook disabled — invalid %s=%q: %v", envPixInterWebhookActor, actorRaw, err)
		return nil
	}
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: pix.inter webhook disabled (DATABASE_URL unset)")
		return nil
	}
	redisURL := getenv(envRedisURL)
	if redisURL == "" {
		log.Printf("crm: pix.inter webhook disabled (REDIS_URL unset)")
		return nil
	}

	verifier, err := pixinter.NewWebhookVerifier(pixinter.WebhookConfig{
		Secret:          secret,
		SignatureHeader: strings.TrimSpace(getenv(envPixInterWebhookSigHeader)),
	})
	if err != nil {
		log.Printf("crm: pix.inter webhook disabled — verifier: %v", err)
		return nil
	}

	pool, err := dial(ctx, dsn)
	if err != nil {
		log.Printf("crm: pix.inter webhook disabled — pg connect: %v", err)
		return nil
	}

	rdb, err := redisDial(redisURL)
	if err != nil {
		pool.Close()
		log.Printf("crm: pix.inter webhook disabled — redis dial: %v", err)
		return nil
	}

	repo, err := pgpix.NewRepository(pool, actor)
	if err != nil {
		_ = rdb.Close()
		pool.Close()
		log.Printf("crm: pix.inter webhook disabled — repository: %v", err)
		return nil
	}
	eventLog, err := pgpix.NewEventLogStore(pool, actor)
	if err != nil {
		_ = rdb.Close()
		pool.Close()
		log.Printf("crm: pix.inter webhook disabled — eventlog: %v", err)
		return nil
	}

	reconciler := domainpix.NewReconciler(repo, eventLog, actor)
	limiter := rlredis.New(rdb, pixInterRateRedisPrefix)
	allowed := parsePixInterAllowedCIDRs(getenv)

	handler, err := httppix.NewInterWebhookHandler(httppix.InterWebhookConfig{
		Verifier:                verifier,
		Parser:                  pixinter.NewWebhookParser(),
		Reconciler:              reconciler,
		Limiter:                 limiter,
		AllowedCIDRs:            allowed,
		IPCheckDisabled:         pixInterIPCheckDisabled(getenv),
		RatePerIPPerMin:         readPositiveInt(getenv(envPixInterWebhookIPRate), 100),
		RatePerExternalIDPerMin: readPositiveInt(getenv(envPixInterWebhookExtRate), 5),
		Logger:                  slog.Default(),
	})
	if err != nil {
		_ = rdb.Close()
		pool.Close()
		log.Printf("crm: pix.inter webhook disabled — handler: %v", err)
		return nil
	}

	log.Printf("crm: pix.inter webhook mounted on public listener (allowlist=%d cidrs, ip_check_enabled=%v)",
		len(allowed), !pixInterIPCheckDisabled(getenv))

	register := func(mux *http.ServeMux) {
		handler.Register(mux)
	}
	cleanup := func() {
		_ = rdb.Close()
		pool.Close()
	}
	return &pixInterWebhookWiring{Register: register, Cleanup: cleanup}
}

// parsePixInterAllowedCIDRs reads PIX_INTER_WEBHOOK_IP_ALLOWLIST and
// falls back to DefaultAllowedCIDRSet when unset. Empty after parse =
// deny-by-default at the handler (the handler also short-circuits when
// the allowlist is empty + IPCheckDisabled is false).
func parsePixInterAllowedCIDRs(getenv func(string) string) []*net.IPNet {
	raw := strings.TrimSpace(getenv(envPixInterWebhookAllowlist))
	if raw == "" {
		return pixinter.DefaultAllowedCIDRSet()
	}
	parsed := pixinter.ParseCIDRList(raw)
	if len(parsed) == 0 {
		log.Printf("crm: pix.inter webhook allowlist parse drained to empty — falling back to defaults")
		return pixinter.DefaultAllowedCIDRSet()
	}
	return parsed
}

// pixInterIPCheckDisabled reads PIX_INTER_WEBHOOK_IP_CHECK and reports
// whether the IP allowlist should be bypassed. Default (unset / "1" /
// "true") keeps the check ON; "0" / "false" disables it.
func pixInterIPCheckDisabled(getenv func(string) string) bool {
	raw := strings.ToLower(strings.TrimSpace(getenv(envPixInterWebhookIPCheck)))
	switch raw {
	case "0", "false", "off", "no":
		return true
	default:
		return false
	}
}

// readPositiveInt parses raw as a positive int; non-positive / unparseable
// falls back to fallback so a misconfig defaults to the stricter posture.
func readPositiveInt(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}
