package instagram

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Env var names. META_APP_SECRET signs the inbound webhook (shared with
// the WhatsApp adapter on the same Meta app); META_INSTAGRAM_VERIFY_TOKEN
// is echoed back during Meta's GET subscription handshake. Both are
// required; an empty value at startup is a misconfiguration the
// composition root surfaces by skipping the adapter wire.
const (
	EnvAppSecret              = "META_APP_SECRET"
	EnvVerifyToken            = "META_INSTAGRAM_VERIFY_TOKEN"
	EnvInstagramEnabled       = "FEATURE_INSTAGRAM_ENABLED"
	EnvInstagramTenantAllow   = "FEATURE_INSTAGRAM_TENANTS"
	EnvInstagramRateMax       = "INSTAGRAM_RATE_MAX_PER_MIN"
	defaultRateMaxPerMinute   = 600
	defaultMaxBodyBytes       = 1 << 20 // 1 MiB
	defaultReplayWindowPast   = 24 * time.Hour
	defaultReplayWindowSkew   = time.Minute
	defaultDeliverTimeoutSec  = 4
	defaultOutboundWindowHour = 24 * time.Hour
)

// Channel is the canonical channel identifier this adapter registers
// under. The string is referenced by tenant_channel_associations rows
// and by F2-06 contacts.IdentityRepository.Resolve when the adapter
// hands an inbound message to the inbox use case.
const Channel = "instagram"

// Config bundles env-driven runtime settings. Secrets (AppSecret,
// VerifyToken) are kept as plain strings; the package never logs them.
type Config struct {
	AppSecret      string
	VerifyToken    string
	RateMaxPerMin  int
	MaxBodyBytes   int64
	PastWindow     time.Duration
	FutureSkew     time.Duration
	DeliverTimeout time.Duration
	// OutboundWindow is Meta's hard-coded customer-care window for
	// Instagram (24h since last user message). The sender refuses to
	// deliver outside the window unless the caller passes an explicit
	// MESSAGE_TAG override (not modelled in Fase 2).
	OutboundWindow time.Duration
}

// ConfigFromEnv reads runtime configuration from getenv. Returns a
// typed error when a required secret is missing so cmd/server can log
// "Instagram disabled — META_APP_SECRET missing" and skip the wire
// without crashing.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, errors.New("instagram: getenv is nil")
	}
	secret := strings.TrimSpace(getenv(EnvAppSecret))
	if secret == "" {
		return Config{}, errors.New("instagram: " + EnvAppSecret + " is empty")
	}
	verify := strings.TrimSpace(getenv(EnvVerifyToken))
	if verify == "" {
		return Config{}, errors.New("instagram: " + EnvVerifyToken + " is empty")
	}
	return Config{
		AppSecret:      secret,
		VerifyToken:    verify,
		RateMaxPerMin:  defaultRateMaxPerMinute,
		MaxBodyBytes:   defaultMaxBodyBytes,
		PastWindow:     defaultReplayWindowPast,
		FutureSkew:     defaultReplayWindowSkew,
		DeliverTimeout: time.Duration(defaultDeliverTimeoutSec) * time.Second,
		OutboundWindow: defaultOutboundWindowHour,
	}, nil
}

// EnvFeatureFlag is the env-driven FeatureFlag implementation.
// FEATURE_INSTAGRAM_ENABLED gates the channel globally (set to "1" to
// turn it on); FEATURE_INSTAGRAM_TENANTS is a comma-separated allowlist
// of tenant UUIDs. An empty allowlist with the global flag on means
// "all tenants enabled". Invalid UUIDs are silently dropped.
type EnvFeatureFlag struct {
	globalOn bool
	allowed  map[uuid.UUID]struct{}
}

// NewEnvFeatureFlag parses the two FEATURE_INSTAGRAM_* env vars.
func NewEnvFeatureFlag(getenv func(string) string) *EnvFeatureFlag {
	if getenv == nil {
		return &EnvFeatureFlag{}
	}
	on := strings.TrimSpace(getenv(EnvInstagramEnabled)) == "1"
	allow := map[uuid.UUID]struct{}{}
	for _, raw := range strings.Split(getenv(EnvInstagramTenantAllow), ",") {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		id, err := uuid.Parse(s)
		if err != nil {
			continue
		}
		allow[id] = struct{}{}
	}
	return &EnvFeatureFlag{globalOn: on, allowed: allow}
}

// Enabled implements FeatureFlag. globalOn=false → always off.
// globalOn with empty allowlist → always on. globalOn with a populated
// allowlist → on iff tenantID is listed.
func (f *EnvFeatureFlag) Enabled(_ context.Context, tenantID uuid.UUID) (bool, error) {
	if f == nil || !f.globalOn {
		return false, nil
	}
	if len(f.allowed) == 0 {
		return true, nil
	}
	_, ok := f.allowed[tenantID]
	return ok, nil
}
