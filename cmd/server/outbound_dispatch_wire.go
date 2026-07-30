package main

// SIN-68306 wiring — outbound WhatsApp Cloud dispatcher.
//
// Closes the gap messenger_wire.go documents ("the outbound Sender is
// constructed during assembly but exposed only to the send-outbound
// dispatcher when that dispatcher is wired (follow-up)"): it builds the
// Meta Cloud Sender (internal/adapter/channel/whatsapp) and wraps it in
// the production decorator stack from internal/adapter/channel/dispatch —
//
//	Router{"whatsapp": Idempotent(RateLimited(sender))}
//
// so the send-outbound use case can talk to a single inbox.OutboundChannel
// that routes by conversation channel, enforces the per-minute carrier
// budget, and is idempotent per operator compose.
//
// Deny-by-default / reversibility: when META_GRAPH_TOKEN is unset (or the
// Sender fails to construct) the builder returns an empty Router — a
// graceful no-op that answers every send with inbox.ErrChannelDisabled and
// issues zero carrier HTTP, so wiring the dispatcher flag-off is safe.
//
// Injection point: the outbound dispatcher is assembled here and retained
// by assembleWhatsAppAdapter; a live SendOutbound consumes it when the
// real inbox channel provider is wired (SIN-63793 W3 follow-up), mirroring
// the messenger_wire.go retain-for-follow-up pattern.
//
// Idempotency ledger: MemoryLedger is in-process (single replica). A
// Redis/Postgres-backed dispatch.Ledger that spans replicas is the
// documented follow-up; the port makes it a drop-in.

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/adapter/channel/dispatch"
	channelwhatsapp "github.com/pericles-luz/crm/internal/adapter/channel/whatsapp"
	channelswhatsapp "github.com/pericles-luz/crm/internal/adapter/channels/whatsapp"
	rlredis "github.com/pericles-luz/crm/internal/adapter/ratelimit/redis"
	"github.com/pericles-luz/crm/internal/inbox"
)

// defaultOutboundRateMaxPerMin mirrors the inbound default in
// channels/whatsapp when WHATSAPP_RATE_MAX_PER_MIN is unset or invalid.
const defaultOutboundRateMaxPerMin = 600

// buildWhatsAppOutbound assembles the outbound WhatsApp dispatcher from
// already-connected dependencies. It always returns a non-nil
// inbox.OutboundChannel: a real routed dispatcher when META_GRAPH_TOKEN is
// present, or an empty Router no-op otherwise, so the caller never has to
// nil-guard and boot never fails.
func buildWhatsAppOutbound(getenv func(string) string, pool *pgxpool.Pool, rdb *goredis.Client, flag *channelswhatsapp.EnvFeatureFlag) inbox.OutboundChannel {
	token := getenv("META_GRAPH_TOKEN")
	if token == "" {
		log.Printf("crm: whatsapp outbound dispatcher disabled (META_GRAPH_TOKEN unset) — no-op router")
		return dispatch.NewRouter(nil)
	}
	lookup := channelwhatsapp.TenantConfigLookup(func(ctx context.Context, tenantID uuid.UUID) (channelwhatsapp.TenantConfig, error) {
		pn, err := whatsappOutboundPhoneNumberID(ctx, pool, tenantID)
		if err != nil {
			return channelwhatsapp.TenantConfig{}, err
		}
		on, flagErr := flag.Enabled(ctx, tenantID)
		if flagErr != nil {
			return channelwhatsapp.TenantConfig{}, flagErr
		}
		return channelwhatsapp.TenantConfig{PhoneNumberID: pn, Enabled: on}, nil
	})
	sender, err := channelwhatsapp.New(token, lookup, prometheus.DefaultRegisterer)
	if err != nil {
		log.Printf("crm: whatsapp outbound dispatcher disabled — %v", err)
		return dispatch.NewRouter(nil)
	}
	var limiter dispatch.RateLimiter
	if rdb != nil {
		limiter = rlredis.New(rdb, "whatsapp:out:")
	}
	log.Printf("crm: whatsapp outbound dispatcher ready")
	return assembleWhatsAppOutbound(sender, limiter, outboundRateMaxPerMin(getenv))
}

// assembleWhatsAppOutbound wraps a carrier sender in the outbound decorator
// stack and registers it under the WhatsApp channel key. Split out from
// buildWhatsAppOutbound so unit tests can wire a fake sender + limiter
// without dialling Postgres/Redis or registering prometheus metrics.
func assembleWhatsAppOutbound(sender inbox.OutboundChannel, limiter dispatch.RateLimiter, rateMax int) inbox.OutboundChannel {
	var oc inbox.OutboundChannel = sender
	// Rate limit before idempotency's carrier call, but inside the
	// idempotency claim so a deduped resend never consumes budget.
	oc = dispatch.NewRateLimited(oc, limiter, time.Minute, rateMax, nil)
	oc = dispatch.NewIdempotent(oc, dispatch.NewMemoryLedger())
	return dispatch.NewRouter(map[string]inbox.OutboundChannel{channelswhatsapp.Channel: oc})
}

// whatsappOutboundPhoneNumberID returns the Meta phone_number_id associated
// with a tenant for outbound sends, from tenant_channel_associations with
// channel="whatsapp" (reverse of the inbound resolver direction). No row
// means the tenant has not configured WhatsApp; the empty phone_number_id
// surfaces as inbox.ErrChannelAuthFailed inside the Sender.
func whatsappOutboundPhoneNumberID(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID) (string, error) {
	const sql = `
		SELECT association
		  FROM tenant_channel_associations
		 WHERE channel = $1 AND tenant_id = $2
		 LIMIT 1`
	var pn string
	err := pool.QueryRow(ctx, sql, channelswhatsapp.Channel, tenantID).Scan(&pn)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return pn, nil
}

// outboundRateMaxPerMin reads WHATSAPP_RATE_MAX_PER_MIN, falling back to
// defaultOutboundRateMaxPerMin for an unset or non-positive value.
func outboundRateMaxPerMin(getenv func(string) string) int {
	raw := strings.TrimSpace(getenv(channelswhatsapp.EnvWhatsAppRateMax))
	if raw == "" {
		return defaultOutboundRateMaxPerMin
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultOutboundRateMaxPerMin
	}
	return n
}
