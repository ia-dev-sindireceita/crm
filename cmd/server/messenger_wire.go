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
// Outbound path (OutboundChannel / SendMessage):
//   - Resolves per-tenant PageID via a reverse lookup against
//     tenant_channel_associations with channel="messenger" (single-key pattern,
//     same as WhatsApp uses for phone_number_id in the same table).
//   - META_GRAPH_TOKEN is the system-user token (shared with WhatsApp sender).
//
// Both paths gate on META_APP_SECRET and DATABASE_URL; any missing dep
// logs a "disabled" line and returns nil (fail-soft, same as whatsapp_wire).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	channelmessenger "github.com/pericles-luz/crm/internal/adapter/channel/messenger"
	"github.com/pericles-luz/crm/internal/adapter/channels/messenger"
	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// messengerWiring bundles the artifacts buildMessengerWiring produces.
// The outbound Sender is constructed during assembly but exposed only to
// the send-outbound dispatcher when that dispatcher is wired (follow-up
// issue covers Messenger + WhatsApp dispatcher alignment).
type messengerWiring struct {
	Register func(*http.ServeMux)
	Cleanup  func()
}

// buildMessengerWiring assembles the production Messenger adapter (inbound
// webhook + outbound sender). Returns nil when any required env var is
// missing — the caller treats nil as "skip mounting Messenger routes".
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
	adapter, sender, cleanup, err := assembleMessengerAdapter(cfg, pool, getenv)
	if err != nil {
		pool.Close()
		log.Printf("crm: messenger intake disabled — assemble: %v", err)
		return nil
	}
	register := func(mux *http.ServeMux) {
		adapter.Register(mux)
	}
	log.Printf("crm: messenger intake mounted on public listener")
	_ = sender // retained for future dispatcher wiring; not yet consumed (mirrors whatsapp_wire.go)
	return &messengerWiring{Register: register, Cleanup: cleanup}
}

// assembleMessengerAdapter constructs the adapter from already-connected
// dependencies. Split out so unit tests can wire fakes instead of real
// pgxpool clients.
func assembleMessengerAdapter(cfg messenger.Config, pool *pgxpool.Pool, getenv func(string) string) (*messenger.Adapter, *channelmessenger.Sender, func(), error) {
	contactsStore, err := pgcontacts.New(pool)
	if err != nil {
		return nil, nil, nil, err
	}
	inboxStore, err := pginbox.New(pool)
	if err != nil {
		return nil, nil, nil, err
	}
	contactsUC, err := contactsusecase.New(contactsStore)
	if err != nil {
		return nil, nil, nil, err
	}
	receiver, err := inboxusecase.NewReceiveInbound(inboxStore, inboxStore, contactsUC)
	if err != nil {
		return nil, nil, nil, err
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

	inboundAdapter, err := messenger.New(cfg, receiver, resolver, flag,
		messenger.WithLogger(slog.Default()),
		// MediaScanPublisher wired in a follow-up PR that lands the
		// NATS-backed publisher in cmd/server.
	)
	if err != nil {
		return nil, nil, nil, err
	}

	// OutboundChannel sender — requires META_GRAPH_TOKEN.
	var outboundSender *channelmessenger.Sender
	graphToken := getenv("META_GRAPH_TOKEN")
	if graphToken != "" {
		tenantConfig := channelmessenger.TenantConfigLookup(func(ctx context.Context, tenantID uuid.UUID) (channelmessenger.TenantConfig, error) {
			pageID, err := messengerOutboundPageID(ctx, pool, tenantID)
			if err != nil {
				return channelmessenger.TenantConfig{}, fmt.Errorf("messenger outbound config: %w", err)
			}
			on, flagErr := flag.Enabled(ctx, tenantID)
			if flagErr != nil {
				return channelmessenger.TenantConfig{}, flagErr
			}
			return channelmessenger.TenantConfig{PageID: pageID, Enabled: on}, nil
		})
		outboundSender, err = channelmessenger.New(graphToken, tenantConfig, prometheus.DefaultRegisterer)
		if err != nil {
			log.Printf("crm: messenger outbound sender disabled — %v", err)
		} else {
			log.Printf("crm: messenger outbound sender ready")
		}
	} else {
		log.Printf("crm: messenger outbound sender disabled (META_GRAPH_TOKEN unset)")
	}

	cleanup := func() { pool.Close() }
	return inboundAdapter, outboundSender, cleanup, nil
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
