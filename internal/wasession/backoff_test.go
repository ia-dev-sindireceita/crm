package wasession

import (
	"testing"
	"time"
)

func TestBackoffDelaySequence(t *testing.T) {
	t.Parallel()
	b := Backoff{Base: time.Second, Max: 10 * time.Second}
	want := []time.Duration{
		time.Second,      // attempt 1
		2 * time.Second,  // attempt 2
		4 * time.Second,  // attempt 3
		8 * time.Second,  // attempt 4
		10 * time.Second, // attempt 5 capped
		10 * time.Second, // attempt 6 capped
	}
	for i, w := range want {
		if got := b.Delay(i + 1); got != w {
			t.Errorf("Delay(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestBackoffAttemptFloor(t *testing.T) {
	t.Parallel()
	b := Backoff{Base: time.Second, Max: time.Minute}
	if got := b.Delay(0); got != time.Second {
		t.Errorf("Delay(0) = %v, want base %v", got, time.Second)
	}
	if got := b.Delay(-5); got != time.Second {
		t.Errorf("Delay(-5) = %v, want base %v", got, time.Second)
	}
}

func TestBackoffZeroFieldsUseDefaults(t *testing.T) {
	t.Parallel()
	var b Backoff // zero value
	if got := b.Delay(1); got != DefaultBackoff.Base {
		t.Errorf("Delay(1) with zero base = %v, want %v", got, DefaultBackoff.Base)
	}
	// Large attempt clamps to the default max.
	if got := b.Delay(100); got != DefaultBackoff.Max {
		t.Errorf("Delay(100) with zero max = %v, want %v", got, DefaultBackoff.Max)
	}
}
