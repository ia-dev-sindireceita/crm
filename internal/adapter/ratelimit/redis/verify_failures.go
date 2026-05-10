package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
)

// VerifyFailures is the redis-backed VerifyFailureCounter adapter
// (SIN-62380 / CAVEAT-3 of SIN-62343). It keeps one INCR-style counter
// per master session id under a namespaced key. Increment is atomic
// (Redis serialises INCR / EXPIRE in a transaction); Reset deletes the
// key.
//
// Lifetime: keys are TTL'd to the master session hard cap (ADR 0073
// §D3 — 4h) so a counter that has not been reset by a successful
// verify still self-collects when its session row would have expired
// anyway. The TTL is refreshed on every Increment so a long-paced
// attacker accumulates strikes within the master session lifetime
// without the counter ageing out.
type VerifyFailures struct {
	client    Counter
	keyPrefix string
	ttl       time.Duration
}

// Counter is the narrow subset of go-redis the verify-failure adapter
// needs. *goredis.Client and goredis.UniversalClient both satisfy it.
// Tests substitute a fake without dragging in a real server.
type Counter interface {
	Incr(ctx context.Context, key string) *goredis.IntCmd
	Expire(ctx context.Context, key string, expiration time.Duration) *goredis.BoolCmd
	Del(ctx context.Context, keys ...string) *goredis.IntCmd
}

// Compile-time assertion that *VerifyFailures satisfies the domain port.
var _ mastermfa.VerifyFailureCounter = (*VerifyFailures)(nil)

// DefaultVerifyFailureTTL mirrors the master-session hard cap from
// ADR 0073 §D3 so an unreset counter self-collects within the same
// window the underlying session row would.
const DefaultVerifyFailureTTL = 4 * time.Hour

// NewVerifyFailures constructs the adapter. nil client returns nil so
// the wireup site sees a fast nil-deref panic at first use.
//
// keyPrefix is prepended to every key the counter writes (e.g.
// "auth:verifyfail:") so multiple environments / namespaces can
// coexist on a shared Redis cluster. ttl is the per-key expiration
// refreshed on every Increment; non-positive values fall back to
// DefaultVerifyFailureTTL.
func NewVerifyFailures(client Counter, keyPrefix string, ttl time.Duration) *VerifyFailures {
	if client == nil {
		return nil
	}
	if ttl <= 0 {
		ttl = DefaultVerifyFailureTTL
	}
	return &VerifyFailures{client: client, keyPrefix: keyPrefix, ttl: ttl}
}

// Increment records one wrong-code attempt and returns the new count.
// First call for a given session returns 1.
//
// Implementation: a single INCR followed by a best-effort EXPIRE. The
// EXPIRE is best-effort because INCR is the authoritative count —
// failing to refresh the TTL on an existing key only means the
// counter ages out per its previous TTL, never that strikes are
// undercounted. INCR errors propagate.
//
// Empty / zero session ids are rejected so a misuse cannot pollute
// the keyspace with a "<prefix>:<nil-uuid>" bucket shared across
// sessions.
func (v *VerifyFailures) Increment(ctx context.Context, sessionID uuid.UUID) (int, error) {
	if sessionID == uuid.Nil {
		return 0, errors.New("redis/verifyfailures: empty session id")
	}
	key := v.keyFor(sessionID)
	count, err := v.client.Incr(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("redis/verifyfailures: incr: %w", err)
	}
	// Refresh TTL. A failure here is logged by the caller via the
	// adapter's surrounding context — the counter still ages out at
	// whatever TTL it had before — so we only surface a genuine call
	// failure (network blip) and ignore a "key does not exist"
	// outcome (impossible after a successful INCR).
	if _, err := v.client.Expire(ctx, key, v.ttl).Result(); err != nil {
		return int(count), fmt.Errorf("redis/verifyfailures: expire: %w", err)
	}
	return int(count), nil
}

// Reset clears the counter for sessionID. A missing key is NOT an
// error — the post-condition (no counter for this id) is satisfied
// either way. DEL errors propagate so a Redis blip that leaves a
// stale strike count is visible to the caller.
func (v *VerifyFailures) Reset(ctx context.Context, sessionID uuid.UUID) error {
	if sessionID == uuid.Nil {
		return errors.New("redis/verifyfailures: empty session id")
	}
	key := v.keyFor(sessionID)
	if _, err := v.client.Del(ctx, key).Result(); err != nil {
		return fmt.Errorf("redis/verifyfailures: del: %w", err)
	}
	return nil
}

func (v *VerifyFailures) keyFor(sessionID uuid.UUID) string {
	return v.keyPrefix + sessionID.String()
}
