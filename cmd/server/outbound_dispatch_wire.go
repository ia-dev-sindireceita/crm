package main

// SIN-68306 wiring — outbound WhatsApp Cloud dispatcher entry.
//
// Builds the Meta Cloud Sender (internal/adapter/channel/whatsapp) and
// wraps it in the production decorator stack from
// internal/adapter/channel/dispatch — Idempotent(RateLimited(sender)) —
// WITHOUT wrapping it in its own single-entry Router. inbox_wire_real.go
// merges this entry with the Messenger / Instagram / fake-customer
// entries into ONE combined dispatch.Router built once per boot, so a
// conversation on any channel with no wired entry fails closed with
// inbox.ErrChannelDisabled (Router's deny-by-default) instead of each
// channel needing its own nested router.
//
// Deny-by-default / reversibility: when META_GRAPH_TOKEN is unset (or the
// Sender fails to construct) the builder returns ok=false — the caller
// omits the WhatsApp entry from the combined route map entirely.
//
// Idempotency ledger: MemoryLedger is in-process (single replica). A
// Redis/Postgres-backed dispatch.Ledger that spans replicas is the
// documented follow-up; the port makes it a drop-in.

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
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

// buildWhatsAppOutboundEntry builds the decorated (idempotent +
// rate-limited) WhatsApp sender WITHOUT wrapping it in a Router, so
// inbox_wire_real.go can merge it with the Messenger / Instagram /
// fake-customer entries into one combined Router built once per boot.
// ok=false means WhatsApp outbound is disabled (no token / construction
// error) — the caller should omit the entry from any combined route map.
//
// This is the ONLY construction site for channelwhatsapp.Sender in the
// real-provider path — do not also build one elsewhere; a second
// construction site would register the whatsapp_send_* Prometheus
// collectors twice and panic at boot (see whatsapp_wire.go's doc comment
// for the incident this guards against).
func buildWhatsAppOutboundEntry(getenv func(string) string, pool *pgxpool.Pool, rdb *goredis.Client, flag *channelswhatsapp.EnvFeatureFlag) (inbox.OutboundChannel, bool) {
	token := getenv("META_GRAPH_TOKEN")
	if token == "" {
		log.Printf("crm: whatsapp outbound dispatcher disabled (META_GRAPH_TOKEN unset)")
		return nil, false
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
		return nil, false
	}
	var limiter dispatch.RateLimiter
	if rdb != nil {
		limiter = rlredis.New(rdb, "whatsapp:out:")
	}
	log.Printf("crm: whatsapp outbound dispatcher ready")
	return assembleWhatsAppOutboundEntry(sender, limiter, outboundRateMaxPerMin(getenv)), true
}

// assembleWhatsAppOutboundEntry wraps a carrier sender in the outbound
// decorator stack (rate-limit, then idempotency). Split out from
// buildWhatsAppOutboundEntry so unit tests can wire a fake sender +
// limiter without dialling Postgres/Redis or registering prometheus
// metrics. The caller is responsible for keying this entry under
// channelswhatsapp.Channel in a combined dispatch.Router — this function
// does not wrap the result in its own single-entry Router (that
// responsibility lives in inbox_wire_real.go's combined route map so
// WhatsApp, Messenger, and Instagram share one Router instead of each
// channel needing its own).
func assembleWhatsAppOutboundEntry(sender inbox.OutboundChannel, limiter dispatch.RateLimiter, rateMax int) inbox.OutboundChannel {
	var oc inbox.OutboundChannel = sender
	// Rate limit before idempotency's carrier call, but inside the
	// idempotency claim so a deduped resend never consumes budget.
	oc = dispatch.NewRateLimited(oc, limiter, time.Minute, rateMax, nil)
	oc = dispatch.NewIdempotent(oc, dispatch.NewMemoryLedger())
	return oc
}

// whatsappOutboundPhoneNumberID (the per-tenant Meta phone_number_id
// resolver from tenant_channel_associations) is defined once in
// whatsapp_outbound_wire.go (SIN-68302) and shared by both outbound
// builders in this package. SIN-68306 originally shipped an identical copy
// here; SIN-67470 W3 consolidated the two into the single canonical
// definition so the cmd/server package builds (the duplicate declaration
// was a merge-order collision between PR #488 and PR #489).

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
