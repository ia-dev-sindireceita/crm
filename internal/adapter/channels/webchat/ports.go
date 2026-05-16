package webchat

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Channel is the identifier for this adapter in the identity and dedup
// ledgers.
const Channel = "webchat"

const (
	EnvEnabled     = "FEATURE_WEBCHAT_ENABLED"
	EnvTenantAllow = "FEATURE_WEBCHAT_TENANTS"

	HeaderSession = "X-Webchat-Session"
	HeaderCSRF    = "X-Webchat-CSRF"

	sessionTTL = 30 * time.Minute
)

// ErrSessionNotFound is returned when the requested session does not
// exist in the store.
var ErrSessionNotFound = errors.New("webchat: session not found")

// Session holds the server-side state for one anonymous visitor window.
type Session struct {
	ID            string
	TenantID      uuid.UUID
	CSRFTokenHash string // sha256(csrf_token), base64url-encoded
	OriginSig     string // HMAC-SHA256(tenant_origin_secret, canonical_origin)
	ExpiresAt     time.Time
}

// FeatureFlag answers whether the webchat channel is enabled for a
// given tenant. Flag-off returns false, nil.
type FeatureFlag interface {
	Enabled(ctx context.Context, tenantID uuid.UUID) (bool, error)
}

// OriginValidator validates that an origin is permitted for a tenant
// (CORS allowlist, ADR-0021 D2) and computes the HMAC origin signature
// (ADR-0021 D4).
type OriginValidator interface {
	Valid(ctx context.Context, tenantID uuid.UUID, origin string) (bool, error)
	HMAC(ctx context.Context, tenantID uuid.UUID, origin string) (string, error)
}

// SessionStore persists visitor sessions.
type SessionStore interface {
	Create(ctx context.Context, s Session) error
	Get(ctx context.Context, sessionID string) (Session, error)
	Touch(ctx context.Context, sessionID string) error
}

// RateLimiter enforces per-key limits. Allow returns whether the
// request is permitted; if not, retryAfter says when to retry.
type RateLimiter interface {
	Allow(ctx context.Context, key string) (allowed bool, retryAfter time.Duration, err error)
}

// ContactSignalUpdater is called when the visitor supplies email or
// phone after their session starts, triggering an identity re-resolve
// and potential merge (ADR-0021 D6).
type ContactSignalUpdater interface {
	UpdateSignals(ctx context.Context, tenantID uuid.UUID, sessionID, phone, email string) error
}

// InMemorySessionStore is the default SessionStore for tests and
// composition roots that have not yet wired the Postgres adapter.
type InMemorySessionStore struct {
	mu sync.RWMutex
	m  map[string]Session
}

// NewInMemorySessionStore returns an empty in-memory store.
func NewInMemorySessionStore() *InMemorySessionStore {
	return &InMemorySessionStore{m: make(map[string]Session)}
}

func (s *InMemorySessionStore) Create(_ context.Context, sess Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[sess.ID] = sess
	return nil
}

func (s *InMemorySessionStore) Get(_ context.Context, id string) (Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.m[id]
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	return sess, nil
}

func (s *InMemorySessionStore) Touch(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return ErrSessionNotFound
	}
	sess.ExpiresAt = time.Now().UTC().Add(sessionTTL)
	s.m[id] = sess
	return nil
}

// InMemoryRateLimiter is a naïve token-bucket for unit tests. It
// always allows in production-like use where max == 0.
type InMemoryRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]int
	max     int
}

// NewInMemoryRateLimiter returns a limiter that permits up to max
// calls per key. max == 0 means unlimited.
func NewInMemoryRateLimiter(max int) *InMemoryRateLimiter {
	return &InMemoryRateLimiter{buckets: make(map[string]int), max: max}
}

func (l *InMemoryRateLimiter) Allow(_ context.Context, key string) (bool, time.Duration, error) {
	if l.max == 0 {
		return true, 0, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buckets[key]++
	if l.buckets[key] > l.max {
		return false, time.Minute, nil
	}
	return true, 0, nil
}
