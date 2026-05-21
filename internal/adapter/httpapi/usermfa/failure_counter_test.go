package usermfa

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMemoryFailureCounterIncrementResetsAfterTTL(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: now}
	c := NewMemoryFailureCounter(time.Minute).WithClock(clock.Now)
	id := uuid.New()

	count, err := c.Increment(context.Background(), id)
	if err != nil {
		t.Fatalf("first increment: %v", err)
	}
	if count != 1 {
		t.Fatalf("first count: want 1 got %d", count)
	}

	count, err = c.Increment(context.Background(), id)
	if err != nil {
		t.Fatalf("second increment: %v", err)
	}
	if count != 2 {
		t.Fatalf("second count: want 2 got %d", count)
	}

	// Advance clock past the TTL — next increment should restart at 1.
	clock.advance(time.Hour)
	count, err = c.Increment(context.Background(), id)
	if err != nil {
		t.Fatalf("post-ttl increment: %v", err)
	}
	if count != 1 {
		t.Fatalf("post-ttl count: want 1 got %d", count)
	}
}

func TestMemoryFailureCounterReset(t *testing.T) {
	t.Parallel()
	c := NewMemoryFailureCounter(time.Minute)
	id := uuid.New()
	for i := 0; i < 3; i++ {
		if _, err := c.Increment(context.Background(), id); err != nil {
			t.Fatalf("increment: %v", err)
		}
	}
	if err := c.Reset(context.Background(), id); err != nil {
		t.Fatalf("reset: %v", err)
	}
	count, err := c.Increment(context.Background(), id)
	if err != nil {
		t.Fatalf("post-reset increment: %v", err)
	}
	if count != 1 {
		t.Fatalf("post-reset count: want 1 got %d", count)
	}
}

func TestMemoryFailureCounterTracksIndependently(t *testing.T) {
	t.Parallel()
	c := NewMemoryFailureCounter(time.Minute)
	a := uuid.New()
	b := uuid.New()
	if _, err := c.Increment(context.Background(), a); err != nil {
		t.Fatalf("a: %v", err)
	}
	count, err := c.Increment(context.Background(), b)
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if count != 1 {
		t.Fatalf("independent count: want 1 got %d", count)
	}
}

type fakeClock struct {
	t time.Time
}

func (c *fakeClock) Now() time.Time { return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}
