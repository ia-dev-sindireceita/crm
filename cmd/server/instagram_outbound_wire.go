package main

// Instagram outbound wiring — OutboundChannel sender for the Instagram
// Direct channel, mirroring messenger_wire.go's buildMessengerOutboundEntry
// almost exactly.
//
// Called exactly once, from inbox_wire_real.go's combined outbound
// Router — NOT from assembleInstagramAdapter (inbound never needs it).
// Building it twice would register the instagram_send_* Prometheus
// collectors twice and panic at boot — the same class of bug fixed for
// WhatsApp (whatsapp_wire.go's dead duplicate construction) and guarded
// against for Messenger.

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
	channelinstagram "github.com/pericles-luz/crm/internal/adapter/channel/instagram"
	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
	rlredis "github.com/pericles-luz/crm/internal/adapter/ratelimit/redis"
	"github.com/pericles-luz/crm/internal/inbox"
)

// buildInstagramOutboundEntry builds the decorated (idempotent +
// rate-limited) Instagram sender WITHOUT wrapping it in a Router, so
// inbox_wire_real.go can merge it with the WhatsApp / Messenger /
// fake-customer entries into one combined Router built once per boot.
// ok=false means Instagram outbound is disabled (no token / construction
// error) — the caller should omit the entry from any combined route map.
//
// This is the ONLY construction site for channelinstagram.Sender — do
// not also build one inside assembleInstagramAdapter (inbound doesn't
// need it); see the file doc comment for why a second construction site
// panics at boot.
func buildInstagramOutboundEntry(getenv func(string) string, pool *pgxpool.Pool, rdb *goredis.Client, flag *instagram.EnvFeatureFlag) (inbox.OutboundChannel, bool) {
	token := instagramOutboundGraphToken(getenv)
	if token == "" {
		log.Printf("crm: instagram outbound sender disabled (META_INSTAGRAM_GRAPH_TOKEN / META_GRAPH_TOKEN unset)")
		return nil, false
	}
	lookup := channelinstagram.TenantConfigLookup(func(ctx context.Context, tenantID uuid.UUID) (channelinstagram.TenantConfig, error) {
		igBusinessID, err := instagramOutboundIGBusinessID(ctx, pool, tenantID)
		if err != nil {
			return channelinstagram.TenantConfig{}, err
		}
		on, flagErr := flag.Enabled(ctx, tenantID)
		if flagErr != nil {
			return channelinstagram.TenantConfig{}, flagErr
		}
		return channelinstagram.TenantConfig{IGBusinessID: igBusinessID, Enabled: on}, nil
	})
	sender, err := channelinstagram.New(token, lookup, prometheus.DefaultRegisterer)
	if err != nil {
		log.Printf("crm: instagram outbound sender disabled — %v", err)
		return nil, false
	}
	var limiter dispatch.RateLimiter
	if rdb != nil {
		limiter = rlredis.New(rdb, "instagram:out:")
	}
	log.Printf("crm: instagram outbound sender ready")
	var oc inbox.OutboundChannel = sender
	oc = dispatch.NewRateLimited(oc, limiter, time.Minute, instagramOutboundRateMaxPerMin(getenv), nil)
	oc = dispatch.NewIdempotent(oc, dispatch.NewMemoryLedger())
	return oc, true
}

// envInstagramGraphToken optionally overrides META_GRAPH_TOKEN for
// Instagram sends, mirroring envMessengerGraphToken's rationale: the
// Instagram professional account can be authorized under a different
// Business Manager / system user than WhatsApp.
const envInstagramGraphToken = "META_INSTAGRAM_GRAPH_TOKEN"

// instagramOutboundGraphToken reads META_INSTAGRAM_GRAPH_TOKEN, falling
// back to the shared META_GRAPH_TOKEN.
func instagramOutboundGraphToken(getenv func(string) string) string {
	if token := strings.TrimSpace(getenv(envInstagramGraphToken)); token != "" {
		return token
	}
	return getenv("META_GRAPH_TOKEN")
}

// defaultInstagramRateMaxPerMin mirrors defaultMessengerRateMaxPerMin
// for the Instagram outbound channel.
const defaultInstagramRateMaxPerMin = 600

// envInstagramOutboundRateMaxPerMin is DELIBERATELY distinct from
// instagram.EnvInstagramRateMax ("INSTAGRAM_RATE_MAX_PER_MIN"), which
// already governs the INBOUND per-ig_business_id webhook rate limiter
// (internal/adapter/channels/instagram/config.go). Reusing that name
// here would silently couple two independent budgets (inbound flood
// protection vs. outbound send throughput) that operators need to tune
// separately.
const envInstagramOutboundRateMaxPerMin = "INSTAGRAM_OUTBOUND_RATE_MAX_PER_MIN"

// instagramOutboundRateMaxPerMin reads envInstagramOutboundRateMaxPerMin,
// falling back to defaultInstagramRateMaxPerMin for an unset or
// non-positive value.
func instagramOutboundRateMaxPerMin(getenv func(string) string) int {
	raw := strings.TrimSpace(getenv(envInstagramOutboundRateMaxPerMin))
	if raw == "" {
		return defaultInstagramRateMaxPerMin
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultInstagramRateMaxPerMin
	}
	return n
}

// instagramOutboundIGBusinessID returns the Instagram Business Account
// id associated with a tenant for outbound sends. It queries
// tenant_channel_associations with channel="instagram" and tenant_id=$1
// (reverse of the inbound direction) — the same table + row the inbound
// resolver reads in the opposite direction, populated by
// /settings/channels via ChannelAssociationWriter.SaveAssociation.
//
// When no row exists the tenant has not configured an Instagram
// account; the caller receives ErrChannelAuthFailed (via
// instagramOutboundIGBusinessID → TenantConfig.IGBusinessID=="").
func instagramOutboundIGBusinessID(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID) (string, error) {
	const sql = `
		SELECT association
		  FROM tenant_channel_associations
		 WHERE channel = $1 AND tenant_id = $2
		 LIMIT 1`
	var igBusinessID string
	err := pool.QueryRow(ctx, sql, instagram.Channel, tenantID).Scan(&igBusinessID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil // caller surfaces as ErrChannelAuthFailed via empty IGBusinessID
	}
	if err != nil {
		return "", err
	}
	return igBusinessID, nil
}
