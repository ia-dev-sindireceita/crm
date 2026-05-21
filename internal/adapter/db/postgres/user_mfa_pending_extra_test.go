package postgres_test

// SIN-63224 follow-up to SIN-63222: pick up the uncovered validation
// branches in user_mfa_pending.go (Create/Get/Delete fast-rejects,
// IsExpired, WithClock, and the clock fallback) without modifying the
// existing TestTenantUserMFAPending_CreateGetDelete suite. New tests
// only — per the engineering quality bar, existing tests are read-only.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
)

func TestPendingMFASession_IsExpired(t *testing.T) {
	t.Parallel()

	expires := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{name: "before expiry", now: expires.Add(-time.Second), want: false},
		{name: "exactly at expiry", now: expires, want: true},
		{name: "after expiry", now: expires.Add(time.Second), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := postgres.PendingMFASession{ExpiresAt: expires}
			if got := p.IsExpired(tc.now); got != tc.want {
				t.Fatalf("IsExpired(%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}

func TestNewTenantUserMFAPending_RejectsZeroTenant(t *testing.T) {
	t.Parallel()
	// Existing TestNewTenantUserMFAPending_RejectsBadInputs covers
	// the nil-pool branch; this adds the zero-tenant branch without
	// touching that test. Non-nil zero-value pool is enough — the
	// constructor only checks for nil and returns before touching it
	// (same pattern as TestWithTenant_RejectsBadArgs in
	// withtenant_test.go).
	_, err := postgres.NewTenantUserMFAPending(&pgxpool.Pool{}, uuid.Nil)
	if !errors.Is(err, postgres.ErrZeroTenant) {
		t.Fatalf("err = %v, want ErrZeroTenant", err)
	}
}

// TestTenantUserMFAPending_RejectsBadInputs covers the Create/Get/Delete
// fast-reject branches that return before any database call. These are
// pure validation paths (uuid.Nil + non-positive ttl) and run without
// hitting Postgres at all, so the zero-value pool is fine.
func TestTenantUserMFAPending_RejectsBadInputs(t *testing.T) {
	t.Parallel()

	tenant := uuid.New()
	a, err := postgres.NewTenantUserMFAPending(&pgxpool.Pool{}, tenant)
	if err != nil {
		t.Fatalf("NewTenantUserMFAPending: %v", err)
	}
	ctx := context.Background()

	t.Run("Create rejects nil userID", func(t *testing.T) {
		t.Parallel()
		_, err := a.Create(ctx, uuid.Nil, 5*time.Minute, "/inbox")
		if err == nil {
			t.Fatal("expected error for nil userID")
		}
		if !strings.Contains(err.Error(), "userID is nil") {
			t.Fatalf("err = %v, want message containing 'userID is nil'", err)
		}
	})

	t.Run("Create rejects zero ttl", func(t *testing.T) {
		t.Parallel()
		_, err := a.Create(ctx, uuid.New(), 0, "/inbox")
		if err == nil {
			t.Fatal("expected error for zero ttl")
		}
		if !strings.Contains(err.Error(), "ttl must be positive") {
			t.Fatalf("err = %v, want message containing 'ttl must be positive'", err)
		}
	})

	t.Run("Create rejects negative ttl", func(t *testing.T) {
		t.Parallel()
		_, err := a.Create(ctx, uuid.New(), -time.Second, "/inbox")
		if err == nil {
			t.Fatal("expected error for negative ttl")
		}
		if !strings.Contains(err.Error(), "ttl must be positive") {
			t.Fatalf("err = %v, want message containing 'ttl must be positive'", err)
		}
	})

	t.Run("Get returns ErrPendingMFANotFound for nil id", func(t *testing.T) {
		t.Parallel()
		_, err := a.Get(ctx, uuid.Nil)
		if !errors.Is(err, postgres.ErrPendingMFANotFound) {
			t.Fatalf("err = %v, want ErrPendingMFANotFound", err)
		}
	})

	t.Run("Delete is a no-op for nil id", func(t *testing.T) {
		t.Parallel()
		if err := a.Delete(ctx, uuid.Nil); err != nil {
			t.Fatalf("Delete(uuid.Nil) = %v, want nil (idempotent)", err)
		}
	})
}

// TestTenantUserMFAPending_WithClockOverridesExpiry exercises both
// WithClock (the copy-with-clock builder) and the now-not-nil branch
// of the private clock() helper, by asserting that the persisted
// ExpiresAt equals the frozen time + ttl.
func TestTenantUserMFAPending_WithClockOverridesExpiry(t *testing.T) {
	db := freshDBWithUserMFA(t)
	tenant, user := seedTenantUser(t, db, "acme-pending-clock.crm.local", "admin@acme-pending-clock.test")
	ctx := context.Background()

	base, err := postgres.NewTenantUserMFAPending(db.RuntimePool(), tenant)
	if err != nil {
		t.Fatalf("NewTenantUserMFAPending: %v", err)
	}

	frozen := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	a := base.WithClock(func() time.Time { return frozen })
	if a == base {
		t.Fatal("WithClock must return a copy, not mutate the receiver")
	}

	row, err := a.Create(ctx, user, 7*time.Minute, "/inbox")
	if err != nil {
		t.Fatalf("Create with WithClock: %v", err)
	}
	want := frozen.Add(7 * time.Minute).UTC()
	if !row.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v (frozen clock + ttl)", row.ExpiresAt, want)
	}

	// And the original adapter is untouched: its expires_at must be
	// near time.Now(), not the frozen value. Use the same DB to
	// avoid bringing up a second one.
	rowNow, err := base.Create(ctx, user, time.Minute, "/inbox")
	if err != nil {
		t.Fatalf("Create on base: %v", err)
	}
	if rowNow.ExpiresAt.Before(time.Now().Add(-time.Minute)) || rowNow.ExpiresAt.After(time.Now().Add(2*time.Minute)) {
		t.Fatalf("base ExpiresAt = %v, want close to time.Now() + 1m", rowNow.ExpiresAt)
	}
}
