package wa_session

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Env var names. The session channel is opt-in per tenant: the global
// flag turns the transport on at all, and the allowlist scopes it to
// named tenants. WA_SESSION_RATE_MAX_PER_MIN caps outbound sends per
// tenant per minute (ban-risk mitigation, ADR 0107 §risk).
// WA_SESSION_INBOUND_RATE_MAX_PER_MIN caps inbound events accepted per
// tenant per minute — SIN-66262 F1: Receive triggers DB writes (contact
// upsert, conversation find/create, message insert) via HandleInbound,
// so an unbounded inbound stream (a high-volume paired session or a
// defective session redelivery loop) is a per-tenant DB load
// amplification vector (OWASP API4). Outbound was already capped; this
// gives inbound the symmetric guard SecurityEngineer requested.
const (
	EnvSessionEnabled              = "FEATURE_WA_SESSION_ENABLED"
	EnvSessionTenantAllow          = "FEATURE_WA_SESSION_TENANTS"
	EnvSessionRateMax              = "WA_SESSION_RATE_MAX_PER_MIN"
	EnvSessionInboundRateMax       = "WA_SESSION_INBOUND_RATE_MAX_PER_MIN"
	defaultRateMaxPerMinute        = 60
	defaultInboundRateMaxPerMinute = 120
	defaultDeliverTimeout          = 8 * time.Second
)

// Config bundles the values safely loaded from env. It carries no
// secrets — the whatsmeow session credentials live in the Fase 1 store,
// never here.
type Config struct {
	RateMaxPerMin        int
	InboundRateMaxPerMin int
	DeliverTimeout       time.Duration
}

// DefaultConfig returns the baseline configuration used when no env
// override is supplied.
func DefaultConfig() Config {
	return Config{
		RateMaxPerMin:        defaultRateMaxPerMinute,
		InboundRateMaxPerMin: defaultInboundRateMaxPerMinute,
		DeliverTimeout:       defaultDeliverTimeout,
	}
}

// ConfigFromEnv reads the runtime configuration from getenv. Unlike the
// official adapter there are no required secrets, so this never errors;
// a missing rate var falls back to the default cap.
func ConfigFromEnv(getenv func(string) string) Config {
	cfg := DefaultConfig()
	if getenv == nil {
		return cfg
	}
	if raw := strings.TrimSpace(getenv(EnvSessionRateMax)); raw != "" {
		if n, err := parsePositiveInt(raw); err == nil {
			cfg.RateMaxPerMin = n
		}
	}
	if raw := strings.TrimSpace(getenv(EnvSessionInboundRateMax)); raw != "" {
		if n, err := parsePositiveInt(raw); err == nil {
			cfg.InboundRateMaxPerMin = n
		}
	}
	return cfg
}

// parsePositiveInt parses a base-10 positive integer; zero, negatives,
// overflow and non-numeric input are rejected so a typo cannot silently
// disable the rate cap (max <= 0 would mean "deny everything" or
// "unbounded" depending on the limiter).
func parsePositiveInt(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errBadInt
		}
		n = n*10 + int(c-'0')
		if n > 1_000_000 {
			return 0, errBadInt
		}
	}
	if n == 0 {
		return 0, errBadInt
	}
	return n, nil
}

var errBadInt = &configError{"wa_session: invalid integer"}

type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }

// EnvFeatureFlag is the default FeatureFlag implementation, shaped like
// the official adapter's: a global on/off plus an optional tenant
// allowlist. Deny-by-default — globalOn=false is always off.
type EnvFeatureFlag struct {
	globalOn bool
	allowed  map[uuid.UUID]struct{}
}

// NewEnvFeatureFlag parses FEATURE_WA_SESSION_ENABLED ("1" enables the
// transport) and FEATURE_WA_SESSION_TENANTS (comma-separated tenant
// UUID allowlist). globalOn with an empty allowlist means "all tenants
// enabled"; invalid UUIDs are silently dropped.
func NewEnvFeatureFlag(getenv func(string) string) *EnvFeatureFlag {
	if getenv == nil {
		return &EnvFeatureFlag{}
	}
	on := strings.TrimSpace(getenv(EnvSessionEnabled)) == "1"
	allow := map[uuid.UUID]struct{}{}
	for _, raw := range strings.Split(getenv(EnvSessionTenantAllow), ",") {
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

// Enabled implements FeatureFlag. globalOn=false → off. globalOn with an
// empty allowlist → on for every tenant. globalOn with a populated
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

// InMemoryRateLimiter is the default RateLimiter: a fixed-window counter
// keyed by string, resetting when window elapses. It is process-local
// (no Redis) — Fase 3 swaps a shared limiter in the composition root.
// Safe for concurrent use.
type InMemoryRateLimiter struct {
	mu    sync.Mutex
	now   func() time.Time
	state map[string]*window
}

type window struct {
	start time.Time
	count int
}

// NewInMemoryRateLimiter returns an empty limiter using the wall clock.
func NewInMemoryRateLimiter() *InMemoryRateLimiter {
	return &InMemoryRateLimiter{now: time.Now, state: map[string]*window{}}
}

// Allow implements RateLimiter. max <= 0 denies everything (a misconfig
// guard); otherwise up to max hits are permitted per window per key.
func (l *InMemoryRateLimiter) Allow(_ context.Context, key string, win time.Duration, max int) (bool, time.Duration, error) {
	if max <= 0 {
		return false, win, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.state[key]
	if w == nil || now.Sub(w.start) >= win {
		l.state[key] = &window{start: now, count: 1}
		return true, 0, nil
	}
	if w.count >= max {
		return false, win - now.Sub(w.start), nil
	}
	w.count++
	return true, 0, nil
}
