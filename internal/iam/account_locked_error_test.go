package iam

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestAccountLockedError_Is_MatchesSentinel(t *testing.T) {
	t.Parallel()
	until := time.Now().Add(15 * time.Minute)
	err := &AccountLockedError{Until: until}
	if !errors.Is(err, ErrAccountLocked) {
		t.Fatal("errors.Is(err, ErrAccountLocked) = false, want true")
	}
}

func TestAccountLockedError_Is_DoesNotMatchOtherSentinels(t *testing.T) {
	t.Parallel()
	err := &AccountLockedError{Until: time.Now().Add(time.Minute)}
	for _, target := range []error{
		ErrInvalidCredentials,
		ErrSessionExpired,
		ErrSessionNotFound,
		ErrInvalidEncoding,
		ErrTenantNotFound,
	} {
		if errors.Is(err, target) {
			t.Fatalf("errors.Is(err, %v) = true, want false", target)
		}
	}
}

func TestAccountLockedError_As_Extracts(t *testing.T) {
	t.Parallel()
	until := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	original := &AccountLockedError{Until: until}
	wrapped := fmt.Errorf("wrap: %w", original)

	var got *AccountLockedError
	if !errors.As(wrapped, &got) {
		t.Fatal("errors.As(wrapped, &got) = false, want true")
	}
	if !got.Until.Equal(until) {
		t.Fatalf("Until = %v, want %v", got.Until, until)
	}
}

func TestAccountLockedError_Error_MatchesSentinel(t *testing.T) {
	t.Parallel()
	err := &AccountLockedError{Until: time.Now()}
	if err.Error() != ErrAccountLocked.Error() {
		t.Fatalf("Error() = %q, want %q", err.Error(), ErrAccountLocked.Error())
	}
}

func TestAccountLockedError_RetryAfter_Future(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	err := (&AccountLockedError{Until: now.Add(900 * time.Second)}).WithClock(func() time.Time { return now })
	got := err.RetryAfter()
	if got != 900*time.Second {
		t.Fatalf("RetryAfter() = %v, want %v", got, 900*time.Second)
	}
}

func TestAccountLockedError_RetryAfter_PastClampsToZero(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	err := (&AccountLockedError{Until: now.Add(-10 * time.Second)}).WithClock(func() time.Time { return now })
	if got := err.RetryAfter(); got != 0 {
		t.Fatalf("past Until: RetryAfter() = %v, want 0", got)
	}
}

func TestAccountLockedError_RetryAfter_ExactlyNowClampsToZero(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	err := (&AccountLockedError{Until: now}).WithClock(func() time.Time { return now })
	if got := err.RetryAfter(); got != 0 {
		t.Fatalf("Until == now: RetryAfter() = %v, want 0", got)
	}
}

func TestAccountLockedError_RetryAfter_ZeroUntil(t *testing.T) {
	t.Parallel()
	err := &AccountLockedError{}
	if got := err.RetryAfter(); got != 0 {
		t.Fatalf("zero Until: RetryAfter() = %v, want 0", got)
	}
}

func TestAccountLockedError_RetryAfter_NilReceiver(t *testing.T) {
	t.Parallel()
	var err *AccountLockedError
	if got := err.RetryAfter(); got != 0 {
		t.Fatalf("nil receiver: RetryAfter() = %v, want 0", got)
	}
}

func TestAccountLockedError_RetryAfter_DefaultClock(t *testing.T) {
	t.Parallel()
	err := &AccountLockedError{Until: time.Now().Add(2 * time.Hour)}
	got := err.RetryAfter()
	// Allow 1s of test-machine jitter on either side.
	if got < (2*time.Hour-time.Second) || got > 2*time.Hour {
		t.Fatalf("RetryAfter() = %v, want ~2h", got)
	}
}

func TestNewAccountLockedError_SetsUntil(t *testing.T) {
	t.Parallel()
	until := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	err := NewAccountLockedError(until)
	if err == nil {
		t.Fatal("NewAccountLockedError returned nil")
	}
	if !err.Until.Equal(until) {
		t.Fatalf("Until = %v, want %v", err.Until, until)
	}
}

func TestAccountLockedError_WithClock_DoesNotMutateOriginal(t *testing.T) {
	t.Parallel()
	now := time.Now()
	original := &AccountLockedError{Until: now.Add(time.Hour)}
	overridden := original.WithClock(func() time.Time { return now.Add(2 * time.Hour) })

	if got := original.RetryAfter(); got <= 0 {
		t.Fatalf("original.RetryAfter() = %v, want > 0", got)
	}
	if got := overridden.RetryAfter(); got != 0 {
		t.Fatalf("overridden.RetryAfter() = %v, want 0 (clock past Until)", got)
	}
}
