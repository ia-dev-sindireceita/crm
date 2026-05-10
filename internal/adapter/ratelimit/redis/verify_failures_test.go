package redis_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/google/uuid"

	rladapter "github.com/pericles-luz/crm/internal/adapter/ratelimit/redis"
)

// fakeCounter is an in-memory go-redis Counter substitute for the
// VerifyFailures adapter. It records every call and returns scripted
// errors per-method so the adapter contract is observable from a
// unit test (no real Redis required).
type fakeCounter struct {
	mu sync.Mutex

	values map[string]int

	incrErr   error
	expireErr error
	delErr    error

	incrCalls   int32
	expireCalls int32
	delCalls    int32

	lastIncrKey   string
	lastExpireKey string
	lastExpireTTL time.Duration
	lastDelKeys   []string
}

func newFakeCounter() *fakeCounter {
	return &fakeCounter{values: make(map[string]int)}
}

func (f *fakeCounter) Incr(ctx context.Context, key string) *goredis.IntCmd {
	cmd := goredis.NewIntCmd(ctx, "INCR", key)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.incrCalls++
	f.lastIncrKey = key
	if f.incrErr != nil {
		cmd.SetErr(f.incrErr)
		return cmd
	}
	f.values[key]++
	cmd.SetVal(int64(f.values[key]))
	return cmd
}

func (f *fakeCounter) Expire(ctx context.Context, key string, expiration time.Duration) *goredis.BoolCmd {
	cmd := goredis.NewBoolCmd(ctx, "EXPIRE", key, expiration)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expireCalls++
	f.lastExpireKey = key
	f.lastExpireTTL = expiration
	if f.expireErr != nil {
		cmd.SetErr(f.expireErr)
		return cmd
	}
	cmd.SetVal(true)
	return cmd
}

func (f *fakeCounter) Del(ctx context.Context, keys ...string) *goredis.IntCmd {
	cmd := goredis.NewIntCmd(ctx, "DEL")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delCalls++
	f.lastDelKeys = append([]string{}, keys...)
	if f.delErr != nil {
		cmd.SetErr(f.delErr)
		return cmd
	}
	deleted := 0
	for _, k := range keys {
		if _, ok := f.values[k]; ok {
			delete(f.values, k)
			deleted++
		}
	}
	cmd.SetVal(int64(deleted))
	return cmd
}

func TestVerifyFailures_NewNilClientReturnsNil(t *testing.T) {
	if got := rladapter.NewVerifyFailures(nil, "x:", time.Hour); got != nil {
		t.Errorf("nil client: expected nil adapter, got %v", got)
	}
}

func TestVerifyFailures_DefaultTTLAppliesOnZero(t *testing.T) {
	c := newFakeCounter()
	v := rladapter.NewVerifyFailures(c, "auth:vf:", 0)
	sid := uuid.New()
	if _, err := v.Increment(context.Background(), sid); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if c.lastExpireTTL != rladapter.DefaultVerifyFailureTTL {
		t.Errorf("ttl: got %v want %v", c.lastExpireTTL, rladapter.DefaultVerifyFailureTTL)
	}
}

func TestVerifyFailures_DefaultTTLAppliesOnNegative(t *testing.T) {
	c := newFakeCounter()
	v := rladapter.NewVerifyFailures(c, "auth:vf:", -time.Second)
	sid := uuid.New()
	if _, err := v.Increment(context.Background(), sid); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if c.lastExpireTTL != rladapter.DefaultVerifyFailureTTL {
		t.Errorf("ttl: got %v want %v", c.lastExpireTTL, rladapter.DefaultVerifyFailureTTL)
	}
}

func TestVerifyFailures_IncrementCountsUp(t *testing.T) {
	c := newFakeCounter()
	v := rladapter.NewVerifyFailures(c, "auth:vf:", 5*time.Minute)
	sid := uuid.New()
	for want := 1; want <= 4; want++ {
		got, err := v.Increment(context.Background(), sid)
		if err != nil {
			t.Fatalf("Increment %d: %v", want, err)
		}
		if got != want {
			t.Errorf("Increment %d: got %d want %d", want, got, want)
		}
	}
	if c.incrCalls != 4 {
		t.Errorf("INCR calls: got %d want 4", c.incrCalls)
	}
	if c.expireCalls != 4 {
		t.Errorf("EXPIRE calls: got %d want 4", c.expireCalls)
	}
}

func TestVerifyFailures_IncrementKeyContainsSessionID(t *testing.T) {
	c := newFakeCounter()
	v := rladapter.NewVerifyFailures(c, "auth:vf:", time.Minute)
	sid := uuid.New()
	if _, err := v.Increment(context.Background(), sid); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if !strings.HasPrefix(c.lastIncrKey, "auth:vf:") {
		t.Errorf("key: got %q want prefix auth:vf:", c.lastIncrKey)
	}
	if !strings.Contains(c.lastIncrKey, sid.String()) {
		t.Errorf("key: got %q does not contain session id %s", c.lastIncrKey, sid)
	}
}

func TestVerifyFailures_IncrementRefreshesExpire(t *testing.T) {
	c := newFakeCounter()
	v := rladapter.NewVerifyFailures(c, "auth:vf:", 7*time.Minute)
	sid := uuid.New()
	if _, err := v.Increment(context.Background(), sid); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if c.lastExpireTTL != 7*time.Minute {
		t.Errorf("EXPIRE ttl: got %v want 7m", c.lastExpireTTL)
	}
	if c.lastExpireKey != c.lastIncrKey {
		t.Errorf("EXPIRE key: got %q want %q", c.lastExpireKey, c.lastIncrKey)
	}
}

func TestVerifyFailures_IncrementErrorPropagates(t *testing.T) {
	c := newFakeCounter()
	c.incrErr = errors.New("redis: connection reset")
	v := rladapter.NewVerifyFailures(c, "auth:vf:", time.Minute)
	sid := uuid.New()
	got, err := v.Increment(context.Background(), sid)
	if err == nil {
		t.Fatal("expected error")
	}
	if got != 0 {
		t.Errorf("count on error: got %d want 0", got)
	}
}

func TestVerifyFailures_IncrementExpireErrorPropagatesWithCount(t *testing.T) {
	// EXPIRE failure surfaces an error but still returns the
	// authoritative INCR count so callers can act on the strike
	// before the TTL refresh stalls.
	c := newFakeCounter()
	c.expireErr = errors.New("redis: i/o timeout")
	v := rladapter.NewVerifyFailures(c, "auth:vf:", time.Minute)
	sid := uuid.New()
	got, err := v.Increment(context.Background(), sid)
	if err == nil {
		t.Fatal("expected error on expire failure")
	}
	if got != 1 {
		t.Errorf("count: got %d want 1 (INCR succeeded)", got)
	}
}

func TestVerifyFailures_IncrementRejectsNilUUID(t *testing.T) {
	c := newFakeCounter()
	v := rladapter.NewVerifyFailures(c, "auth:vf:", time.Minute)
	_, err := v.Increment(context.Background(), uuid.Nil)
	if err == nil {
		t.Fatal("expected error on nil session id")
	}
	if c.incrCalls != 0 {
		t.Errorf("INCR ran on nil session id: %d", c.incrCalls)
	}
}

func TestVerifyFailures_ResetDeletesKey(t *testing.T) {
	c := newFakeCounter()
	v := rladapter.NewVerifyFailures(c, "auth:vf:", time.Minute)
	sid := uuid.New()
	if _, err := v.Increment(context.Background(), sid); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if err := v.Reset(context.Background(), sid); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if c.delCalls != 1 {
		t.Errorf("DEL calls: got %d want 1", c.delCalls)
	}
	if len(c.lastDelKeys) != 1 || !strings.Contains(c.lastDelKeys[0], sid.String()) {
		t.Errorf("DEL keys: got %v want one containing %s", c.lastDelKeys, sid)
	}
}

func TestVerifyFailures_ResetMissingKeyIsNoError(t *testing.T) {
	c := newFakeCounter()
	v := rladapter.NewVerifyFailures(c, "auth:vf:", time.Minute)
	if err := v.Reset(context.Background(), uuid.New()); err != nil {
		t.Fatalf("Reset on missing key: %v", err)
	}
}

func TestVerifyFailures_ResetErrorPropagates(t *testing.T) {
	c := newFakeCounter()
	c.delErr = errors.New("redis: ssl handshake")
	v := rladapter.NewVerifyFailures(c, "auth:vf:", time.Minute)
	if err := v.Reset(context.Background(), uuid.New()); err == nil {
		t.Fatal("expected error on DEL failure")
	}
}

func TestVerifyFailures_ResetRejectsNilUUID(t *testing.T) {
	c := newFakeCounter()
	v := rladapter.NewVerifyFailures(c, "auth:vf:", time.Minute)
	if err := v.Reset(context.Background(), uuid.Nil); err == nil {
		t.Fatal("expected error on nil session id")
	}
	if c.delCalls != 0 {
		t.Errorf("DEL ran on nil session id: %d", c.delCalls)
	}
}

func TestVerifyFailures_PerSessionIsolation(t *testing.T) {
	// Two sessions accumulate strikes independently.
	c := newFakeCounter()
	v := rladapter.NewVerifyFailures(c, "auth:vf:", time.Minute)
	a := uuid.New()
	b := uuid.New()
	for i := 0; i < 3; i++ {
		_, _ = v.Increment(context.Background(), a)
	}
	got, err := v.Increment(context.Background(), b)
	if err != nil {
		t.Fatalf("Increment b: %v", err)
	}
	if got != 1 {
		t.Errorf("session b count: got %d want 1 (independent of a)", got)
	}
}
