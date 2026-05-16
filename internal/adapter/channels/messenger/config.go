package messenger

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Channel is the channel identifier the adapter registers under. ADR
// 0075 §D2 requires [a-z0-9_]+ and uniqueness.
const Channel = "messenger"

// Env var names. META_APP_SECRET is shared with the WhatsApp adapter
// (one Meta app signs all product families); the verify token is
// per-channel so operators can rotate Messenger and WhatsApp
// subscriptions independently. The flag pair gates the channel
// globally and by tenant allowlist; both default to "off" so a partial
// rollout never leaks Messenger to tenants that have not opted in.
const (
	EnvAppSecret            = "META_APP_SECRET"
	EnvVerifyToken          = "META_MESSENGER_VERIFY_TOKEN"
	EnvMessengerEnabled     = "FEATURE_MESSENGER_ENABLED"
	EnvMessengerTenantAllow = "FEATURE_MESSENGER_TENANTS"

	defaultMaxBodyBytes = 1 << 20 // 1 MiB
	// defaultReplayWindowPast matches Meta Cloud API's 24h retry
	// budget (same as WhatsApp / ADR 0094 §3). The dedup ledger's
	// 30-day retention (ADR 0089 §4) must outlast this window — the
	// invariant lives on the WhatsApp side and applies here too since
	// both channels share inbound_message_dedup.
	defaultReplayWindowPast  = 24 * time.Hour
	defaultReplayWindowSkew  = time.Minute
	defaultDeliverTimeoutSec = 4
)

// Config bundles everything safely loaded from env. Secrets
// (AppSecret, VerifyToken) are plain strings; the package never logs
// them and never serialises a Config beyond the composition root.
type Config struct {
	AppSecret      string
	VerifyToken    string
	MaxBodyBytes   int64
	PastWindow     time.Duration
	FutureSkew     time.Duration
	DeliverTimeout time.Duration
}

// ConfigFromEnv reads the runtime configuration from getenv. Returns a
// typed error when a required secret is missing so cmd/server can log
// a single "Messenger disabled — META_APP_SECRET missing" line and
// skip the adapter wire without crashing.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, errors.New("messenger: getenv is nil")
	}
	secret := strings.TrimSpace(getenv(EnvAppSecret))
	if secret == "" {
		return Config{}, errors.New("messenger: " + EnvAppSecret + " is empty")
	}
	verify := strings.TrimSpace(getenv(EnvVerifyToken))
	if verify == "" {
		return Config{}, errors.New("messenger: " + EnvVerifyToken + " is empty")
	}
	return Config{
		AppSecret:      secret,
		VerifyToken:    verify,
		MaxBodyBytes:   defaultMaxBodyBytes,
		PastWindow:     defaultReplayWindowPast,
		FutureSkew:     defaultReplayWindowSkew,
		DeliverTimeout: time.Duration(defaultDeliverTimeoutSec) * time.Second,
	}, nil
}

// EnvFeatureFlag is the default FeatureFlag implementation: the
// channel is on iff FEATURE_MESSENGER_ENABLED="1" AND either the
// FEATURE_MESSENGER_TENANTS allowlist is empty or contains the tenant.
// The struct mirrors whatsapp.EnvFeatureFlag — production swaps in a
// DB-backed implementation in a follow-up PR.
type EnvFeatureFlag struct {
	globalOn bool
	allowed  map[uuid.UUID]struct{}
}

// NewEnvFeatureFlag parses the two FEATURE_MESSENGER_* env vars.
// Invalid UUIDs in the allowlist are silently dropped; cmd/server is
// expected to log the input string at startup so operators can spot
// typos.
func NewEnvFeatureFlag(getenv func(string) string) *EnvFeatureFlag {
	if getenv == nil {
		return &EnvFeatureFlag{}
	}
	on := strings.TrimSpace(getenv(EnvMessengerEnabled)) == "1"
	allow := map[uuid.UUID]struct{}{}
	for _, raw := range strings.Split(getenv(EnvMessengerTenantAllow), ",") {
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
// globalOn with an empty allowlist → on for every tenant. globalOn
// with a populated allowlist → on iff tenantID is listed.
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
