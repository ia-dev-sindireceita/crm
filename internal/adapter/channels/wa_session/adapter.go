package wa_session

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
	"github.com/pericles-luz/crm/internal/inbox"
)

// rateWindow is the fixed window the per-tenant outbound rate cap
// (cfg.RateMaxPerMin) is measured over.
const rateWindow = time.Minute

// mediaPlaceholder is the body persisted for a media-only inbound
// message. The domain rejects an empty body (inbox.ErrInvalidBody), so
// rather than drop the customer's message we materialise it with a
// neutral placeholder and HasAttachments=true; the existing pipeline
// then marks media.scan_status="pending". Fetching and scanning the
// actual media bytes is a later-phase concern (the session/security
// phases own the media pipeline) — this border keeps the message
// visible to the operator instead of losing it.
const mediaPlaceholder = "[mídia]"

// Adapter bridges the whatsmeow session (Fase 1) and the inbox domain.
// It is constructed once at startup; no field is mutated afterwards so
// it is safe to share across the session's event goroutines and the
// send-outbound callers.
type Adapter struct {
	inbox  inbox.InboundChannel
	sender SessionSender
	flag   FeatureFlag
	rate   RateLimiter
	logger *slog.Logger
	cfg    Config
}

// Option mutates an Adapter at construction time.
type Option func(*Adapter)

// WithLogger replaces the default slog.Default logger.
func WithLogger(l *slog.Logger) Option {
	return func(a *Adapter) {
		if l != nil {
			a.logger = l
		}
	}
}

// WithConfig overrides the default Config (rate caps, deliver timeout).
// Each field overrides only when positive, so a partially-populated
// Config keeps the package defaults for the fields it leaves at zero.
func WithConfig(cfg Config) Option {
	return func(a *Adapter) {
		if cfg.RateMaxPerMin > 0 {
			a.cfg.RateMaxPerMin = cfg.RateMaxPerMin
		}
		if cfg.InboundRateMaxPerMin > 0 {
			a.cfg.InboundRateMaxPerMin = cfg.InboundRateMaxPerMin
		}
		if cfg.DeliverTimeout > 0 {
			a.cfg.DeliverTimeout = cfg.DeliverTimeout
		}
	}
}

// New validates required dependencies and returns a ready Adapter.
func New(in inbox.InboundChannel, sender SessionSender, flag FeatureFlag, rate RateLimiter, opts ...Option) (*Adapter, error) {
	if in == nil {
		return nil, ErrNilInbound
	}
	if sender == nil {
		return nil, ErrNilSender
	}
	if flag == nil {
		return nil, ErrNilFlag
	}
	if rate == nil {
		return nil, ErrNilRate
	}
	a := &Adapter{
		inbox:  in,
		sender: sender,
		flag:   flag,
		rate:   rate,
		logger: slog.Default(),
		cfg:    DefaultConfig(),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

// Receive translates one carrier-neutral SessionMessage from the
// whatsmeow session into an inbox.InboundEvent and hands it to the
// receive-inbound use case. Border drops (self-echo, group, missing id,
// malformed phone, empty content, channel disabled) are logged and
// return nil — a dropped event is not a session failure. A genuine
// downstream error is returned so the caller can decide whether to
// retry; a domain dedup hit is treated as success.
func (a *Adapter) Receive(ctx context.Context, msg SessionMessage) error {
	if msg.TenantID == uuid.Nil {
		a.logger.Warn("wa_session.drop_no_tenant", slog.String("provider", Provider))
		return nil
	}
	// Self-echo: whatsmeow re-delivers our own outbound as an event.
	if msg.FromMe {
		return nil
	}
	// Group chats are out of inbox v1 scope: a group JID is not an
	// E.164 contact, and unifying it into a 1:1 thread is incorrect.
	if msg.IsGroup {
		a.logger.Debug("wa_session.drop_group",
			slog.String("tenant_id", msg.TenantID.String()))
		return nil
	}
	messageID := strings.TrimSpace(msg.MessageID)
	if messageID == "" {
		a.logger.Warn("wa_session.drop_no_message_id",
			slog.String("tenant_id", msg.TenantID.String()))
		return nil
	}
	sender, err := normalizeWhatsAppPhone(msg.SenderPhone)
	if err != nil {
		a.logger.Warn("wa_session.drop_bad_phone",
			slog.String("tenant_id", msg.TenantID.String()),
			slog.String("message_id", messageID))
		return nil
	}

	enabled, err := a.flag.Enabled(ctx, msg.TenantID)
	if err != nil {
		return err
	}
	if !enabled {
		a.logger.Debug("wa_session.drop_flag_off",
			slog.String("tenant_id", msg.TenantID.String()))
		return nil
	}

	body, hasAttachments := normalizeInboundBody(msg)
	if body == "" {
		// Neither text nor media — nothing to persist.
		a.logger.Debug("wa_session.drop_empty",
			slog.String("tenant_id", msg.TenantID.String()),
			slog.String("message_id", messageID))
		return nil
	}

	// SIN-66262 F1: per-tenant inbound volume cap. Everything below this
	// point reaches HandleInbound, which writes to the DB (contact
	// upsert, conversation find/create, message insert). The check sits
	// after the border drops so malformed/echo/group events that never
	// touch the DB do not consume a tenant's budget; it mirrors the
	// outbound limiter (same RateLimiter port) but on its own key and
	// cap so inbound and outbound throttle independently. Over the cap we
	// reject (no persist) rather than drop silently, bounding the
	// per-tenant DB write amplification from a high-volume or defective
	// redelivering session.
	allowed, _, err := a.rate.Allow(ctx, "wa_session:in:"+msg.TenantID.String(), rateWindow, a.cfg.InboundRateMaxPerMin)
	if err != nil {
		return err
	}
	if !allowed {
		a.logger.Warn("wa_session.inbound_rate_limited",
			slog.String("tenant_id", msg.TenantID.String()),
			slog.String("message_id", messageID))
		return ErrInboundRateLimited
	}

	ev := inbox.InboundEvent{
		TenantID:          msg.TenantID,
		Channel:           Channel,
		ChannelExternalID: messageID,
		SenderExternalID:  sender,
		SenderDisplayName: strings.TrimSpace(msg.SenderName),
		Body:              body,
		OccurredAt:        msg.Timestamp,
		HasAttachments:    hasAttachments,
	}

	deliverCtx := ctx
	if a.cfg.DeliverTimeout > 0 {
		var cancel context.CancelFunc
		deliverCtx, cancel = context.WithTimeout(ctx, a.cfg.DeliverTimeout)
		defer cancel()
	}
	if err := a.inbox.HandleInbound(deliverCtx, ev); err != nil {
		// Domain dedup hit is the success path under redelivery.
		if errors.Is(err, inbox.ErrInboundAlreadyProcessed) {
			a.logger.Debug("wa_session.duplicate",
				slog.String("tenant_id", msg.TenantID.String()),
				slog.String("message_id", messageID))
			return nil
		}
		return err
	}
	return nil
}

// SendMessage implements inbox.OutboundChannel: it validates the
// outbound at the border, enforces the per-tenant feature flag and rate
// limit, then sends via the whatsmeow session, returning the
// library-assigned message id as the channel_external_id.
func (a *Adapter) SendMessage(ctx context.Context, m inbox.OutboundMessage) (string, error) {
	if m.TenantID == uuid.Nil {
		return "", ErrInvalidTenant
	}
	to, err := normalizeWhatsAppPhone(m.ToExternalID)
	if err != nil {
		return "", ErrInvalidRecipient
	}
	body := strings.TrimSpace(m.Body)
	if body == "" {
		return "", ErrEmptyBody
	}

	enabled, err := a.flag.Enabled(ctx, m.TenantID)
	if err != nil {
		return "", err
	}
	if !enabled {
		return "", ErrChannelDisabled
	}

	allowed, _, err := a.rate.Allow(ctx, "wa_session:out:"+m.TenantID.String(), rateWindow, a.cfg.RateMaxPerMin)
	if err != nil {
		return "", err
	}
	if !allowed {
		a.logger.Warn("wa_session.rate_limited",
			slog.String("tenant_id", m.TenantID.String()))
		return "", ErrRateLimited
	}

	sendCtx := ctx
	if a.cfg.DeliverTimeout > 0 {
		var cancel context.CancelFunc
		sendCtx, cancel = context.WithTimeout(ctx, a.cfg.DeliverTimeout)
		defer cancel()
	}
	messageID, err := a.sender.SendText(sendCtx, m.TenantID, to, body)
	if err != nil {
		return "", err
	}
	return messageID, nil
}

// normalizeInboundBody decides the persisted body and the HasAttachments
// flag. Text wins; a media-only message gets the placeholder body and
// HasAttachments=true so it is not lost (see mediaPlaceholder).
func normalizeInboundBody(msg SessionMessage) (body string, hasAttachments bool) {
	if b := strings.TrimSpace(msg.Body); b != "" {
		return b, msg.HasMedia
	}
	if msg.HasMedia {
		return mediaPlaceholder, true
	}
	return "", false
}

// normalizeWhatsAppPhone turns whatsmeow's phone representation (bare
// digits, a '+'-prefixed number, or a full JID like
// "5511999990001@s.whatsapp.net") into the strict E.164 form the domain
// requires, reusing contacts.NewChannelIdentity so the E.164 rule has a
// single source of truth and cannot drift from the contact ledger.
func normalizeWhatsAppPhone(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if i := strings.IndexByte(s, '@'); i >= 0 { // strip JID server part
		s = s[:i]
	}
	if i := strings.IndexByte(s, ':'); i >= 0 { // strip JID device part
		s = s[:i]
	}
	s = strings.TrimPrefix(s, "+")
	if s == "" {
		return "", ErrInvalidRecipient
	}
	id, err := contacts.NewChannelIdentity(contacts.ChannelWhatsApp, "+"+s)
	if err != nil {
		return "", ErrInvalidRecipient
	}
	return id.ExternalID, nil
}

// Compile-time guard: the adapter satisfies the outbound port so the
// composition root (Fase 3) can hand it to the send-outbound use case.
var _ inbox.OutboundChannel = (*Adapter)(nil)
