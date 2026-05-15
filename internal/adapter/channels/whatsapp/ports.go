package whatsapp

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// TenantResolver maps a Meta phone_number_id to its owning tenant. The
// concrete adapter queries the tenant_channel_associations table from
// ADR 0075 — channel = "whatsapp", association = phone_number_id — and
// returns ErrUnknownPhoneNumberID when no row matches. The webhook
// handler treats that error as a silent drop (anti-enumeration) so an
// attacker who spams the endpoint with random phone_number_ids cannot
// distinguish "tenant exists" from "tenant exists but flag off".
type TenantResolver interface {
	Resolve(ctx context.Context, phoneNumberID string) (uuid.UUID, error)
}

// FeatureFlag answers "is the inbound WhatsApp path enabled for this
// tenant?". ADR 0087 D5 mandates this gate: flipping the flag off
// stops Message materialisation immediately while leaving HMAC
// verification and dedup intact. The default implementation in this
// package reads a small env-driven allowlist; production swaps in a
// per-tenant DB-backed implementation in a follow-up PR.
type FeatureFlag interface {
	Enabled(ctx context.Context, tenantID uuid.UUID) (bool, error)
}

// RateLimiter is the per-key counter port. Implementations decide
// whether the next hit fits inside (window, max) and return a
// retryAfter the handler can echo in logs. The whatsapp adapter keys
// limits on phone_number_id so a single misbehaving tenant cannot
// starve neighbours; the limit is generous because Meta itself rate-
// limits at the source (the cap exists only to absorb retry storms).
type RateLimiter interface {
	Allow(ctx context.Context, key string, window time.Duration, max int) (allowed bool, retryAfter time.Duration, err error)
}

// Clock decouples time.Now from the timestamp-window check so unit
// tests can pin "now" without sleeping. Production uses systemClock.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

// Now implements Clock.
func (systemClock) Now() time.Time { return time.Now().UTC() }
