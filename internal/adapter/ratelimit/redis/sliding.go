// Package redis implements ratelimit.RateLimiter on top of Redis using a
// sliding-window counter. The window is kept as a sorted set whose
// members are unique hit ids and whose scores are unix-millisecond
// timestamps.
//
// The whole "trim old / count current / record new" sequence runs
// inside a single Lua script (slidingScript) so two concurrent callers
// cannot race past the cap. The script also computes the suggested
// Retry-After in one round-trip so the HTTP middleware does not need a
// follow-up call.
//
// This is the only place under internal/ratelimit/... that may import
// the redis client; the iam/ratelimit domain port stays storage-
// agnostic per acceptance criterion #8 of SIN-62341.
package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/ratelimit"
)

// Scripter is the narrow subset of the go-redis client surface the
// adapter needs. *goredis.Client and goredis.UniversalClient both
// satisfy it. Tests substitute a fake without dragging in a real
// server.
type Scripter interface {
	EvalSha(ctx context.Context, sha1 string, keys []string, args ...interface{}) *goredis.Cmd
	Eval(ctx context.Context, script string, keys []string, args ...interface{}) *goredis.Cmd
	ScriptLoad(ctx context.Context, script string) *goredis.StringCmd
}

// SlidingWindow is the redis-backed RateLimiter adapter.
type SlidingWindow struct {
	client    Scripter
	keyPrefix string
	scriptSha string
	now       func() time.Time
	newID     func() string
}

// Compile-time assertion that *SlidingWindow satisfies the domain port.
var _ ratelimit.RateLimiter = (*SlidingWindow)(nil)

// New constructs a SlidingWindow. keyPrefix is prepended to every key
// the limiter writes to so multiple environments / namespaces can
// coexist on a shared Redis (e.g. "auth:rl:"). nil client returns nil
// so callers see a fast nil-deref panic at first use.
func New(client Scripter, keyPrefix string) *SlidingWindow {
	if client == nil {
		return nil
	}
	return &SlidingWindow{
		client:    client,
		keyPrefix: keyPrefix,
		now:       time.Now,
		newID:     defaultID,
	}
}

// WithClock overrides the time source. Tests freeze the clock to make
// retry-after assertions deterministic.
func (s *SlidingWindow) WithClock(now func() time.Time) *SlidingWindow {
	cp := *s
	cp.now = now
	return &cp
}

// WithIDFunc overrides the unique-id generator used as ZSET members.
// Tests inject a deterministic counter so EVAL inputs are stable in
// assertions.
func (s *SlidingWindow) WithIDFunc(newID func() string) *SlidingWindow {
	cp := *s
	cp.newID = newID
	return &cp
}

// Allow records a hit on key inside window of length window and reports
// whether the post-record total is below max. The return contract
// matches ratelimit.RateLimiter.
//
// Implementation contract:
//
//   - At most one EVAL round-trip per call (EvalSha when the script is
//     already loaded; Eval as the cold-cache fallback that loads it).
//   - Atomic with respect to concurrent callers — Redis serialises
//     EVAL.
//   - Retry-After is the time until the oldest in-window hit ages out;
//     it is positive on the throttled path and zero on the allowed
//     path.
func (s *SlidingWindow) Allow(ctx context.Context, key string, window time.Duration, max int) (bool, time.Duration, error) {
	if key == "" {
		return false, 0, errors.New("redis/ratelimit: empty key")
	}
	if window <= 0 {
		return false, 0, errors.New("redis/ratelimit: window must be > 0")
	}
	if max <= 0 {
		return false, 0, errors.New("redis/ratelimit: max must be > 0")
	}

	now := s.now()
	args := []interface{}{
		strconv.FormatInt(now.UnixMilli(), 10),
		strconv.FormatInt(window.Milliseconds(), 10),
		strconv.Itoa(max),
		s.newID(),
	}
	keys := []string{s.keyPrefix + key}

	res, err := s.evalScript(ctx, keys, args)
	if err != nil {
		return false, 0, fmt.Errorf("redis/ratelimit: eval: %w", err)
	}
	allowed, retryMs, err := decodeResult(res)
	if err != nil {
		return false, 0, fmt.Errorf("redis/ratelimit: decode: %w", err)
	}
	return allowed, time.Duration(retryMs) * time.Millisecond, nil
}

// evalScript runs the sliding-window script, loading it on the first
// call (NOSCRIPT cold path).
func (s *SlidingWindow) evalScript(ctx context.Context, keys []string, args []interface{}) (interface{}, error) {
	if s.scriptSha != "" {
		v, err := s.client.EvalSha(ctx, s.scriptSha, keys, args...).Result()
		if err == nil {
			return v, nil
		}
		// NOSCRIPT means the script was flushed (or this is a different
		// node). Fall through to Eval which both runs and loads.
		if !isNoScript(err) {
			return nil, err
		}
	}
	v, err := s.client.Eval(ctx, slidingScript, keys, args...).Result()
	if err != nil {
		return nil, err
	}
	// Best-effort cache the SHA for the next call. A failure to load
	// is non-fatal; the next Allow falls back to plain Eval.
	if sha, err := s.client.ScriptLoad(ctx, slidingScript).Result(); err == nil {
		s.scriptSha = sha
	}
	return v, nil
}

// decodeResult turns the Lua return value ({allowed, retry_after_ms})
// into typed values. Redis returns Lua tables as []interface{} of
// int64; we tolerate the alternate shapes go-redis emits across
// versions defensively.
func decodeResult(res interface{}) (bool, int64, error) {
	arr, ok := res.([]interface{})
	if !ok {
		return false, 0, fmt.Errorf("unexpected result type %T", res)
	}
	if len(arr) != 2 {
		return false, 0, fmt.Errorf("expected 2 result values, got %d", len(arr))
	}
	allowedInt, err := toInt64(arr[0])
	if err != nil {
		return false, 0, fmt.Errorf("allowed: %w", err)
	}
	retryMs, err := toInt64(arr[1])
	if err != nil {
		return false, 0, fmt.Errorf("retry_after: %w", err)
	}
	return allowedInt == 1, retryMs, nil
}

func toInt64(v interface{}) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	case string:
		return strconv.ParseInt(x, 10, 64)
	default:
		return 0, fmt.Errorf("unsupported numeric type %T", v)
	}
}

func isNoScript(err error) bool {
	if err == nil {
		return false
	}
	// go-redis surfaces NOSCRIPT as a redis error whose message has
	// the "NOSCRIPT" prefix. Stable across v9 minor releases.
	return len(err.Error()) >= 8 && err.Error()[:8] == "NOSCRIPT"
}

func defaultID() string { return uuid.NewString() }

// slidingScript implements the sliding-window logic.
//
// KEYS[1] = bucket key
// ARGV[1] = now_ms (string-encoded)
// ARGV[2] = window_ms
// ARGV[3] = max
// ARGV[4] = unique member id
//
// Returns {allowed, retry_after_ms} where allowed is 1 or 0.
//
// Behaviour:
//
//  1. Trim members older than now-window.
//  2. If the trimmed count is already at the cap, return {0, retry}
//     where retry is the time the oldest in-window member needs to age
//     out. The new hit is NOT added (rejected hits do not extend the
//     window).
//  3. Otherwise add the new hit, refresh PEXPIRE to 2*window, return
//     {1, 0}.
const slidingScript = `
local key      = KEYS[1]
local now      = tonumber(ARGV[1])
local window   = tonumber(ARGV[2])
local max      = tonumber(ARGV[3])
local member   = ARGV[4]
local cutoff   = now - window

redis.call('ZREMRANGEBYSCORE', key, '-inf', cutoff)
local count = tonumber(redis.call('ZCARD', key))

if count >= max then
  local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
  local retry  = window
  if #oldest >= 2 then
    local oldest_ts = tonumber(oldest[2])
    local age_out   = (oldest_ts + window) - now
    if age_out > 0 then
      retry = age_out
    else
      retry = 1
    end
  end
  return {0, retry}
end

redis.call('ZADD', key, now, member)
redis.call('PEXPIRE', key, window * 2)
return {1, 0}
`
