package main

// SIN-68302 — WhatsApp Cloud-API OUTBOUND sender wiring (dispatcher).
//
// Closes the gap documented in htmx_wire.go and messenger_wire.go: the
// internal/adapter/channel/whatsapp.Sender (Graph /<phone_number_id>/messages)
// compiled but was imported by no binary, so an operator's inbox reply on a
// WhatsApp conversation fell through to the notFoundSendOutbound{} stub
// (inbox_wire.go) and 404'd.
//
// This wire:
//   1. Constructs the channel/whatsapp.Sender gated on META_GRAPH_TOKEN
//      (Model A system-user token) AND the global FEATURE_WHATSAPP flag —
//      deny-by-default, fail-soft (missing token/flag => nil, no outbound,
//      no regression to the inbound intake in whatsapp_wire.go).
//   2. Resolves per-tenant TenantConfig (phone_number_id + enabled) from
//      tenant_channel_associations, mirroring messenger_wire's closure
//      (messengerOutboundPageID, messenger_wire.go:160-206).
//   3. Routes the outbound by the conversation's channel: WhatsApp
//      conversations go to the Graph Sender, every other channel falls
//      through to a primary fallback (channelRoutingOutbound). A tenant with
//      messenger + whatsapp + wa_session therefore never crosses carriers —
//      the same shape as providerRoutingOutbound in wa_session_wire.go.
//   4. Consumes the Sender through a real inbox/usecase.SendOutbound so the
//      authenticated operator reply -> SendMessage(Graph v18.0) path is live,
//      replacing the notFoundSendOutbound{} stub on the disabled inbox
//      provider (the production default). Deny-by-default: token/flag/DSN
//      absent => the stub stays and nothing changes.
//
// Security (secure-by-default / least privilege):
//   - META_GRAPH_TOKEN lives only in the Sender value + Authorization
//     header; it is never logged, never placed in a URL, and never in a
//     returned error (the Sender package guarantees this; sender_test.go
//     covers the redaction). This wire logs tenant ids and readiness lines
//     only — never the token, phone numbers, or message bodies.
//   - Deny-by-default: the global flag off => zero outbound.
//   - No new endpoint: the outbound is triggered by the already-authenticated
//     POST /inbox/conversations/{id}/messages handler.
//
// E2E two-way validation against the live Graph API is operator-gated and
// tracked in SIN-67470; this wire + its unit tests run without a real token.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	waoutbound "github.com/pericles-luz/crm/internal/adapter/channel/whatsapp"
	channelswa "github.com/pericles-luz/crm/internal/adapter/channels/whatsapp"
	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	"github.com/pericles-luz/crm/internal/contacts"
	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// envMetaGraphToken is the Meta system-user (Model A) access token used
// for outbound Graph calls. It is the same token messenger_wire reads —
// both channels live under one Meta app.
const envMetaGraphToken = "META_GRAPH_TOKEN"

// channelRoutingOutbound multiplexes an outbound send onto the carrier
// adapter that matches the conversation's channel. WhatsApp conversations
// (OutboundMessage.Channel == "whatsapp") route to the Graph Sender; every
// other channel falls through to primary. Deny-by-default: a nil WhatsApp
// sender (flag/token off) collapses to primary, so the wrapper is a
// transparent pass-through — the same coexistence contract
// providerRoutingOutbound (wa_session_wire.go) provides for the session
// transport.
type channelRoutingOutbound struct {
	// whatsapp is the Graph outbound Sender. Nil when META_GRAPH_TOKEN or
	// the global FEATURE_WHATSAPP flag is absent — then WhatsApp sends fall
	// through to primary like any other channel.
	whatsapp inbox.OutboundChannel
	// primary handles every non-WhatsApp channel (and WhatsApp too when the
	// Sender is nil). Never nil — the builder supplies disabledOutbound{}
	// when no other carrier is wired, so an unrouteable channel surfaces a
	// clean 404 instead of a nil dereference.
	primary inbox.OutboundChannel
	logger  *slog.Logger
}

// SendMessage implements inbox.OutboundChannel.
func (r channelRoutingOutbound) SendMessage(ctx context.Context, m inbox.OutboundMessage) (string, error) {
	if m.Channel == channelswa.Channel && r.whatsapp != nil {
		return r.whatsapp.SendMessage(ctx, m)
	}
	if r.primary == nil {
		// Defensive: the builder always sets primary, but never panic on a
		// misconfigured wrapper — behave like the disabled stub (404).
		return "", inbox.ErrNotFound
	}
	return r.primary.SendMessage(ctx, m)
}

// disabledOutbound is the deny-by-default fallback for channels with no
// wired outbound carrier. It never sends and never fabricates a success;
// ErrNotFound matches the prior notFoundSendOutbound{} stub so a reply on
// an unsupported channel keeps 404'ing rather than silently 200'ing.
type disabledOutbound struct{}

// SendMessage implements inbox.OutboundChannel.
func (disabledOutbound) SendMessage(context.Context, inbox.OutboundMessage) (string, error) {
	return "", inbox.ErrNotFound
}

// buildWhatsAppOutboundSender constructs the Graph outbound Sender, gated
// on META_GRAPH_TOKEN AND the global FEATURE_WHATSAPP flag. Returns nil
// (fail-soft) when either is absent — the caller treats nil as "no
// WhatsApp outbound", so every send routes to the primary fallback and the
// inbound intake in whatsapp_wire.go is untouched (AC4, no regression).
//
// reg registers the whatsapp_send_* counters; production passes
// prometheus.DefaultRegisterer, tests a fresh registry to avoid duplicate
// registration panics.
func buildWhatsAppOutboundSender(pool *pgxpool.Pool, getenv func(string) string, reg prometheus.Registerer) inbox.OutboundChannel {
	token := strings.TrimSpace(getenv(envMetaGraphToken))
	if token == "" {
		log.Printf("crm: whatsapp outbound disabled (META_GRAPH_TOKEN unset)")
		return nil
	}
	// Global kill-switch: an all-tenants-off flag means zero outbound. The
	// per-tenant allowlist is re-checked inside the TenantConfigLookup, so
	// this env probe is only the cheap global gate (mirrors
	// waSessionGloballyEnabled).
	if strings.TrimSpace(getenv(channelswa.EnvWhatsAppEnabled)) != "1" {
		log.Printf("crm: whatsapp outbound disabled (FEATURE_WHATSAPP_ENABLED != 1)")
		return nil
	}
	flag := channelswa.NewEnvFeatureFlag(getenv)
	phone := func(ctx context.Context, tenantID uuid.UUID) (string, error) {
		return whatsappOutboundPhoneNumberID(ctx, pool, tenantID)
	}
	sender, err := waoutbound.New(token, whatsappTenantConfigLookup(phone, flag), reg)
	if err != nil {
		log.Printf("crm: whatsapp outbound disabled — sender: %v", err)
		return nil
	}
	log.Printf("crm: whatsapp outbound sender ready")
	return sender
}

// phoneNumberLookup resolves a tenant's outbound WhatsApp phone_number_id.
// The production implementation queries tenant_channel_associations
// (whatsappOutboundPhoneNumberID); tests inject a fake so the config
// assembly logic is exercised without a live pool (AC4, no DB in unit test).
type phoneNumberLookup func(ctx context.Context, tenantID uuid.UUID) (string, error)

// whatsappTenantConfigLookup resolves the per-tenant Graph config the
// Sender needs on the send critical path: the phone_number_id and the
// per-tenant enabled flag. It mirrors the messenger_wire.go:160-169 closure
// (reverse-direction association lookup + feature flag), keeping the two
// carriers' per-tenant resolution shape identical. Taking a phoneNumberLookup
// rather than a raw *pgxpool.Pool keeps the flag/phone composition testable.
func whatsappTenantConfigLookup(phone phoneNumberLookup, flag channelswa.FeatureFlag) waoutbound.TenantConfigLookup {
	return func(ctx context.Context, tenantID uuid.UUID) (waoutbound.TenantConfig, error) {
		phoneNumberID, err := phone(ctx, tenantID)
		if err != nil {
			return waoutbound.TenantConfig{}, fmt.Errorf("whatsapp outbound config: %w", err)
		}
		on, flagErr := flag.Enabled(ctx, tenantID)
		if flagErr != nil {
			return waoutbound.TenantConfig{}, flagErr
		}
		return waoutbound.TenantConfig{PhoneNumberID: phoneNumberID, Enabled: on}, nil
	}
}

// whatsappOutboundPhoneNumberID returns the Meta phone_number_id bound to a
// tenant for outbound sends. It queries tenant_channel_associations with
// channel="whatsapp" and tenant_id=$1 (reverse of the inbound direction,
// which resolves phone_number_id -> tenant). Mirrors messengerOutboundPageID.
//
// When no row exists the tenant has not configured a WhatsApp number; the
// caller surfaces ErrChannelAuthFailed via TenantConfig.PhoneNumberID=="".
func whatsappOutboundPhoneNumberID(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID) (string, error) {
	const sql = `
		SELECT association
		  FROM tenant_channel_associations
		 WHERE channel = $1 AND tenant_id = $2
		 LIMIT 1`
	var phoneNumberID string
	err := pool.QueryRow(ctx, sql, channelswa.Channel, tenantID).Scan(&phoneNumberID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil // caller surfaces as ErrChannelAuthFailed via empty PhoneNumberID
	}
	if err != nil {
		return "", err
	}
	return phoneNumberID, nil
}

// conversationResolver is the narrow read port the outbound contact lookup
// needs from inbox storage: conversation -> ContactID. *pginbox.Store
// satisfies it; tests inject a recording fake (accept-narrow).
type conversationResolver interface {
	GetConversation(ctx context.Context, tenantID, conversationID uuid.UUID) (*inbox.Conversation, error)
}

// contactIdentityFinder is the narrow read port the outbound contact lookup
// needs from contacts storage: contact id -> channel identities.
// *pgcontacts.Store satisfies it; tests inject a fake.
type contactIdentityFinder interface {
	FindByID(ctx context.Context, tenantID, id uuid.UUID) (*contacts.Contact, error)
}

// whatsappOutboundContactLookup resolves a conversation to the recipient's
// WhatsApp E.164 external id. SendOutbound calls it when the web handler
// leaves ToExternalID empty — which it always does: handlers.go builds the
// SendOutboundInput with only tenant/conversation/body. It reads the
// conversation's ContactID under the tenant scope, then the contact's
// whatsapp channel identity.
func whatsappOutboundContactLookup(convs conversationResolver, finder contactIdentityFinder) inboxusecase.ContactLookupFn {
	return func(ctx context.Context, tenantID, conversationID uuid.UUID) (string, error) {
		conv, err := convs.GetConversation(ctx, tenantID, conversationID)
		if err != nil {
			return "", err
		}
		c, err := finder.FindByID(ctx, tenantID, conv.ContactID)
		if err != nil {
			return "", err
		}
		for _, id := range c.Identities() {
			if id.Channel == contacts.ChannelWhatsApp {
				return id.ExternalID, nil
			}
		}
		return "", fmt.Errorf("whatsapp outbound: contact %s has no whatsapp identity", conv.ContactID)
	}
}

// passthroughWalletDebitor honours inbox.WalletDebitor by running the
// charge callback with no reservation. SendOutbound's cost function
// defaults to 0 (WithCost unset), so a passthrough is behaviourally exact
// today while still exercising the reserve->charge->commit ordering the
// port documents. Swapping in the tariff-table-backed wallet adapter is a
// separate slice (inbox WalletDebitor PR5); this wire stays decoupled from
// wallet storage.
type passthroughWalletDebitor struct{}

// Debit implements inbox.WalletDebitor.
func (passthroughWalletDebitor) Debit(ctx context.Context, _ uuid.UUID, _ int64, charge func(ctx context.Context) error) error {
	return charge(ctx)
}

// buildInboxOutboundSendForView builds the real outbound send-for-view use
// case backing the disabled inbox provider's POST /inbox/.../messages
// route. Gated deny-by-default on META_GRAPH_TOKEN + FEATURE_WHATSAPP +
// DATABASE_URL. Returns (nil, noop, false) when any gate is closed so the
// caller keeps the notFoundSendOutbound{} stub — no pool is opened and the
// production default is unchanged.
//
// On success it returns a *inboxusecase.SendOutbound wired to route
// WhatsApp-channel conversations to the Graph Sender, plus a cleanup that
// closes the pool the send path owns.
func buildInboxOutboundSendForView(ctx context.Context, getenv func(string) string) (webinbox.SendOutboundUseCase, func(), bool) {
	noop := func() {}
	if strings.TrimSpace(getenv(envMetaGraphToken)) == "" ||
		strings.TrimSpace(getenv(channelswa.EnvWhatsAppEnabled)) != "1" {
		// Deny-by-default: no token or global flag off => keep the stub.
		return nil, noop, false
	}
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: whatsapp outbound disabled (DATABASE_URL unset); inbox send path stays stubbed")
		return nil, noop, false
	}
	pool, err := pgpool.New(ctx, dsn)
	if err != nil {
		log.Printf("crm: whatsapp outbound disabled — pg connect: %v; inbox send path stays stubbed", err)
		return nil, noop, false
	}
	sender := buildWhatsAppOutboundSender(pool, getenv, prometheus.DefaultRegisterer)
	if sender == nil {
		pool.Close()
		return nil, noop, false
	}
	inboxStore, err := pginbox.New(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: whatsapp outbound disabled — inbox store: %v; inbox send path stays stubbed", err)
		return nil, noop, false
	}
	contactsStore, err := pgcontacts.New(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: whatsapp outbound disabled — contacts store: %v; inbox send path stays stubbed", err)
		return nil, noop, false
	}
	routing := channelRoutingOutbound{
		whatsapp: sender,
		primary:  disabledOutbound{},
		logger:   slog.Default(),
	}
	sendUC, err := inboxusecase.NewSendOutbound(
		inboxStore,
		passthroughWalletDebitor{},
		routing,
		inboxusecase.WithContactLookup(whatsappOutboundContactLookup(inboxStore, contactsStore)),
	)
	if err != nil {
		pool.Close()
		log.Printf("crm: whatsapp outbound disabled — send-outbound usecase: %v; inbox send path stays stubbed", err)
		return nil, noop, false
	}
	log.Printf("crm: inbox outbound send path wired to whatsapp Graph sender (provider=disabled read side)")
	return sendUC, func() { pool.Close() }, true
}
