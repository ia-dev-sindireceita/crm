package wa_session

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
)

// Channel is the channel identifier this adapter registers under. It is
// intentionally the SAME string as the official Meta adapter
// (contacts.ChannelWhatsApp) so a contact's WhatsApp thread is unified
// across providers — ADR 0107 D4. convention_test pins this equality.
const Channel = contacts.ChannelWhatsApp

// Provider distinguishes this non-official session transport from the
// official Meta Cloud transport ("meta") when both serve channel
// "whatsapp". ADR 0107 D4: routing is by provider, not by a separate
// channel string.
const Provider = "session"

// SessionMessage is the carrier-neutral inbound event the whatsmeow
// session manager (Fase 1) hands to Adapter.Receive. It is whatsmeow's
// events.Message reduced to exactly the fields the inbox needs, so this
// package stays free of any go.mau.fi/whatsmeow import (see doc.go).
//
// SenderPhone is whatsmeow's JID user part — bare digits, no leading
// '+', and possibly the full JID string ("5511...@s.whatsapp.net");
// the adapter normalises it to strict E.164 before the domain sees it.
// MessageID is whatsmeow's message id and becomes the (channel,
// channel_external_id) dedup key. FromMe and IsGroup are the two border
// drops: an echo of our own send and a group chat (out of inbox v1
// scope — group JIDs are not E.164 contacts).
type SessionMessage struct {
	TenantID    uuid.UUID
	MessageID   string
	SenderPhone string
	SenderName  string
	Body        string
	Timestamp   time.Time
	HasMedia    bool
	FromMe      bool
	IsGroup     bool
}

// SessionSender is the single outbound seam to the whatsmeow session
// (Fase 1). The implementation maps toE164 into a whatsmeow JID and
// calls the lib's SendMessage on the tenant's client, returning the
// library-assigned message id so the send-outbound use case can persist
// it as the message's channel_external_id.
//
// toE164 is always a validated, '+'-prefixed E.164 string — the adapter
// validates at the border, so the implementation may convert it to a
// JID without re-checking shape.
type SessionSender interface {
	SendText(ctx context.Context, tenantID uuid.UUID, toE164, body string) (messageID string, err error)
}

// FeatureFlag answers "is the WhatsApp session channel enabled for this
// tenant?". The session channel is opt-in per tenant (plan §5, ADR 0107
// D4) so the default is deny: an unconfigured tenant is off. Mirrors the
// official adapter's flag port; production swaps a DB-backed impl in a
// later phase.
type FeatureFlag interface {
	Enabled(ctx context.Context, tenantID uuid.UUID) (bool, error)
}

// RateLimiter is the per-key counter port, shaped exactly like the
// official whatsapp adapter's so the same concrete limiter can back
// both. The session adapter keys outbound limits on the tenant id to
// throttle a single tenant's session and reduce ban exposure.
type RateLimiter interface {
	Allow(ctx context.Context, key string, window time.Duration, max int) (allowed bool, retryAfter time.Duration, err error)
}

// Outbound-side validation / policy errors. Inbound border drops are
// not errors (the adapter logs and returns nil so a dropped event never
// looks like a session failure to Fase 1).
var (
	// ErrNilInbound etc. are returned by New when a required dependency
	// is missing — a misconfigured composition root must fail fast.
	ErrNilInbound = errors.New("wa_session: InboundChannel is nil")
	ErrNilSender  = errors.New("wa_session: SessionSender is nil")
	ErrNilFlag    = errors.New("wa_session: FeatureFlag is nil")
	ErrNilRate    = errors.New("wa_session: RateLimiter is nil")

	// ErrInvalidTenant is returned by SendMessage when TenantID is the
	// zero UUID.
	ErrInvalidTenant = errors.New("wa_session: invalid tenant id")
	// ErrInvalidRecipient is returned by SendMessage when ToExternalID
	// is not a valid E.164 number.
	ErrInvalidRecipient = errors.New("wa_session: recipient is not a valid E.164 number")
	// ErrEmptyBody is returned by SendMessage when the body is blank.
	ErrEmptyBody = errors.New("wa_session: outbound body must not be empty")
	// ErrChannelDisabled is returned by SendMessage when the tenant's
	// session channel feature flag is off (deny-by-default).
	ErrChannelDisabled = errors.New("wa_session: channel disabled for tenant")
	// ErrRateLimited is returned by SendMessage when the per-tenant
	// outbound rate limit is exceeded; the send-outbound use case maps
	// it to the message's failed state.
	ErrRateLimited = errors.New("wa_session: outbound rate limit exceeded")
)
