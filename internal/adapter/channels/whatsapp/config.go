package whatsapp

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Env var names. META_APP_SECRET signs every inbound webhook;
// META_VERIFY_TOKEN is echoed back during Meta's GET /webhooks/whatsapp
// subscription handshake. Both are required; an empty value at startup
// is a misconfiguration (cmd/server fails fast).
const (
	EnvAppSecret             = "META_APP_SECRET"
	EnvVerifyToken           = "META_VERIFY_TOKEN"
	EnvWhatsAppEnabled       = "FEATURE_WHATSAPP_ENABLED"
	EnvWhatsAppTenantAllow   = "FEATURE_WHATSAPP_TENANTS"
	EnvWhatsAppRateMax       = "WHATSAPP_RATE_MAX_PER_MIN"
	defaultRateMaxPerMinute  = 600
	defaultMaxBodyBytes      = 1 << 20 // 1 MiB
	defaultReplayWindowPast  = 5 * time.Minute
	defaultReplayWindowSkew  = time.Minute
	defaultDeliverTimeoutSec = 4
)

// Channel is the channel identifier this adapter registers under. ADR
// 0075 §D2 requires [a-z0-9_]+ and uniqueness; the WhatsApp Cloud API
// is the only inbound source under this name.
const Channel = "whatsapp"

// Config bundles everything that's safely loaded from env. Secrets
// (AppSecret, VerifyToken) are kept as plain strings here; the package
// never logs them and never serialises a Config beyond the
// composition root.
type Config struct {
	AppSecret      string
	VerifyToken    string
	RateMaxPerMin  int
	MaxBodyBytes   int64
	PastWindow     time.Duration
	FutureSkew     time.Duration
	DeliverTimeout time.Duration
}

// ConfigFromEnv reads the runtime configuration from getenv. The
// function returns a typed error when a required secret is missing so
// cmd/server can log a single "WhatsApp disabled — META_APP_SECRET
// missing" line and skip the adapter wire without crashing.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, errors.New("whatsapp: getenv is nil")
	}
	secret := strings.TrimSpace(getenv(EnvAppSecret))
	if secret == "" {
		return Config{}, errors.New("whatsapp: " + EnvAppSecret + " is empty")
	}
	verify := strings.TrimSpace(getenv(EnvVerifyToken))
	if verify == "" {
		return Config{}, errors.New("whatsapp: " + EnvVerifyToken + " is empty")
	}
	cfg := Config{
		AppSecret:      secret,
		VerifyToken:    verify,
		RateMaxPerMin:  defaultRateMaxPerMinute,
		MaxBodyBytes:   defaultMaxBodyBytes,
		PastWindow:     defaultReplayWindowPast,
		FutureSkew:     defaultReplayWindowSkew,
		DeliverTimeout: time.Duration(defaultDeliverTimeoutSec) * time.Second,
	}
	return cfg, nil
}

// EnvFeatureFlag is the default FeatureFlag implementation. Two env
// vars compose: FEATURE_WHATSAPP_ENABLED gates the channel globally
// (set to "1" to turn it on at all), and FEATURE_WHATSAPP_TENANTS is a
// comma-separated allowlist of tenant UUIDs. Either an empty global
// flag or a non-empty allowlist that excludes the tenant returns
// (false, nil). The struct is a placeholder for the DB-backed
// implementation tracked in the follow-up PR.
type EnvFeatureFlag struct {
	globalOn bool
	allowed  map[uuid.UUID]struct{}
}

// NewEnvFeatureFlag parses the two FEATURE_WHATSAPP_* env vars into a
// flag implementation. An empty allowlist combined with globalOn=true
// means "all tenants enabled". Invalid UUIDs in the allowlist are
// silently dropped; cmd/server logs the input string at startup so
// operators can spot typos.
func NewEnvFeatureFlag(getenv func(string) string) *EnvFeatureFlag {
	if getenv == nil {
		return &EnvFeatureFlag{}
	}
	on := strings.TrimSpace(getenv(EnvWhatsAppEnabled)) == "1"
	allow := map[uuid.UUID]struct{}{}
	for _, raw := range strings.Split(getenv(EnvWhatsAppTenantAllow), ",") {
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

// Enabled implements FeatureFlag. globalOn=false → always off. globalOn
// with an empty allowlist → always on. globalOn with a populated
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
