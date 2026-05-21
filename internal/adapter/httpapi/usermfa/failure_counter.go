package usermfa

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryFailureCounter is a process-local FailureCounter used by tests
// and single-instance deploys. The map self-collects: a counter that
// has not been touched inside its TTL is dropped on the next read.
//
// Production multi-instance deploys SHOULD replace this with a Redis
// adapter (mirroring internal/adapter/ratelimit/redis) so the strike
// count is shared across replicas. The FailureCounter port stays the
// same; only the wiring changes.
type MemoryFailureCounter struct {
	ttl   time.Duration
	now   func() time.Time
	mu    sync.Mutex
	store map[uuid.UUID]failureEntry
}

type failureEntry struct {
	count    int
	expireAt time.Time
}

// NewMemoryFailureCounter returns a counter whose entries decay after
// ttl. A zero or negative ttl falls back to DefaultLockoutWindow.
func NewMemoryFailureCounter(ttl time.Duration) *MemoryFailureCounter {
	if ttl <= 0 {
		ttl = DefaultLockoutWindow
	}
	return &MemoryFailureCounter{
		ttl:   ttl,
		now:   time.Now,
		store: make(map[uuid.UUID]failureEntry),
	}
}

// WithClock overrides the time source for tests.
func (c *MemoryFailureCounter) WithClock(now func() time.Time) *MemoryFailureCounter {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now != nil {
		c.now = now
	}
	return c
}

// Increment bumps the counter for pendingID and returns the new value.
// An expired entry is reset to 1 transparently.
func (c *MemoryFailureCounter) Increment(_ context.Context, pendingID uuid.UUID) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	entry, ok := c.store[pendingID]
	if !ok || !now.Before(entry.expireAt) {
		entry = failureEntry{count: 0, expireAt: now.Add(c.ttl)}
	}
	entry.count++
	c.store[pendingID] = entry
	return entry.count, nil
}

// Reset drops the counter for pendingID. A missing counter is not an
// error — the post-condition is satisfied either way.
func (c *MemoryFailureCounter) Reset(_ context.Context, pendingID uuid.UUID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.store, pendingID)
	return nil
}
