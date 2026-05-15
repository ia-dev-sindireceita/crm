package wallet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
)

var fixedTime = time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC)

func TestNew_RejectsZeroTenant(t *testing.T) {
	t.Parallel()
	_, err := wallet.New(uuid.Nil, fixedTime)
	if !errors.Is(err, wallet.ErrZeroTenant) {
		t.Fatalf("New(uuid.Nil): got %v, want ErrZeroTenant", err)
	}
}

func TestNew_StartsZeroVersion(t *testing.T) {
	t.Parallel()
	w, err := wallet.New(uuid.New(), fixedTime)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := w.Version(); got != 0 {
		t.Errorf("Version() = %d, want 0", got)
	}
	if got := w.Balance(); got != 0 {
		t.Errorf("Balance() = %d, want 0", got)
	}
	if got := w.Reserved(); got != 0 {
		t.Errorf("Reserved() = %d, want 0", got)
	}
	if got := w.Available(); got != 0 {
		t.Errorf("Available() = %d, want 0", got)
	}
	if w.ID() == uuid.Nil {
		t.Error("ID() = uuid.Nil, want allocated")
	}
}

func TestHydrate_RoundTripsState(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	tid := uuid.New()
	w := wallet.NewHydrator().Hydrate(id, tid, 100, 30, 7, fixedTime, fixedTime.Add(time.Minute))
	if w.ID() != id || w.TenantID() != tid {
		t.Fatalf("ID/TenantID mismatch")
	}
	if w.Balance() != 100 || w.Reserved() != 30 || w.Available() != 70 || w.Version() != 7 {
		t.Errorf("balance/reserved/available/version mismatch: %d/%d/%d/%d", w.Balance(), w.Reserved(), w.Available(), w.Version())
	}
	if !w.CreatedAt().Equal(fixedTime) {
		t.Errorf("CreatedAt = %v, want %v", w.CreatedAt(), fixedTime)
	}
	if !w.UpdatedAt().Equal(fixedTime.Add(time.Minute)) {
		t.Errorf("UpdatedAt = %v, want %v", w.UpdatedAt(), fixedTime.Add(time.Minute))
	}
}

func TestReserve_HappyPath(t *testing.T) {
	t.Parallel()
	w := wallet.NewHydrator().Hydrate(uuid.New(), uuid.New(), 100, 0, 0, fixedTime, fixedTime)
	if err := w.Reserve(40, fixedTime.Add(time.Second)); err != nil {
		t.Fatalf("Reserve(40): %v", err)
	}
	if w.Balance() != 100 || w.Reserved() != 40 || w.Available() != 60 {
		t.Errorf("post-Reserve state: balance=%d reserved=%d available=%d", w.Balance(), w.Reserved(), w.Available())
	}
	if w.Version() != 1 {
		t.Errorf("version after Reserve = %d, want 1", w.Version())
	}
}

func TestReserve_RejectsBadAmount(t *testing.T) {
	t.Parallel()
	cases := []int64{0, -1, -100}
	for _, amt := range cases {
		w := wallet.NewHydrator().Hydrate(uuid.New(), uuid.New(), 100, 0, 0, fixedTime, fixedTime)
		if err := w.Reserve(amt, fixedTime); !errors.Is(err, wallet.ErrInvalidAmount) {
			t.Errorf("Reserve(%d): got %v, want ErrInvalidAmount", amt, err)
		}
		if w.Version() != 0 {
			t.Errorf("Reserve(%d) bumped version on rejected input", amt)
		}
	}
}

func TestReserve_InsufficientFunds(t *testing.T) {
	t.Parallel()
	w := wallet.NewHydrator().Hydrate(uuid.New(), uuid.New(), 50, 20, 1, fixedTime, fixedTime)
	if err := w.Reserve(31, fixedTime); !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("Reserve(31) over available=30: got %v, want ErrInsufficientFunds", err)
	}
	// State must be unchanged on rejection.
	if w.Balance() != 50 || w.Reserved() != 20 || w.Version() != 1 {
		t.Errorf("post-fail state mutated: balance=%d reserved=%d version=%d", w.Balance(), w.Reserved(), w.Version())
	}
}

func TestReserve_ExactlyAvailable(t *testing.T) {
	t.Parallel()
	w := wallet.NewHydrator().Hydrate(uuid.New(), uuid.New(), 50, 20, 0, fixedTime, fixedTime)
	if err := w.Reserve(30, fixedTime); err != nil {
		t.Fatalf("Reserve(30) at exact available: %v", err)
	}
	if w.Available() != 0 {
		t.Errorf("available after exact reserve = %d, want 0", w.Available())
	}
}

func TestCommit_HappyPath(t *testing.T) {
	t.Parallel()
	w := wallet.NewHydrator().Hydrate(uuid.New(), uuid.New(), 100, 40, 5, fixedTime, fixedTime)
	if err := w.Commit(25, 40, fixedTime); err != nil {
		t.Fatalf("Commit(25, reserved=40): %v", err)
	}
	if w.Balance() != 75 {
		t.Errorf("Balance after partial commit = %d, want 75", w.Balance())
	}
	if w.Reserved() != 0 {
		t.Errorf("Reserved after commit = %d, want 0 (full reservation released)", w.Reserved())
	}
	if w.Version() != 6 {
		t.Errorf("Version after commit = %d, want 6", w.Version())
	}
}

func TestCommit_FullReservation(t *testing.T) {
	t.Parallel()
	w := wallet.NewHydrator().Hydrate(uuid.New(), uuid.New(), 100, 40, 5, fixedTime, fixedTime)
	if err := w.Commit(40, 40, fixedTime); err != nil {
		t.Fatalf("Commit(40, 40): %v", err)
	}
	if w.Balance() != 60 || w.Reserved() != 0 {
		t.Errorf("post-Commit state: balance=%d reserved=%d", w.Balance(), w.Reserved())
	}
}

func TestCommit_RejectsBadInput(t *testing.T) {
	t.Parallel()
	type tc struct {
		name           string
		commit         int64
		reservedAt     int64
		walletReserved int64
		want           error
	}
	cases := []tc{
		{"zero commit", 0, 10, 10, wallet.ErrInvalidAmount},
		{"negative commit", -1, 10, 10, wallet.ErrInvalidAmount},
		{"zero reserved", 5, 0, 10, wallet.ErrInvalidAmount},
		{"commit larger than reserved-at-call", 11, 10, 10, wallet.ErrInvalidAmount},
		{"reserved-at-call larger than in-memory reserved", 5, 20, 10, wallet.ErrVersionConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := wallet.NewHydrator().Hydrate(uuid.New(), uuid.New(), 100, c.walletReserved, 0, fixedTime, fixedTime)
			err := w.Commit(c.commit, c.reservedAt, fixedTime)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if w.Version() != 0 {
				t.Errorf("Commit bumped version on rejected input")
			}
		})
	}
}

func TestRelease_HappyPath(t *testing.T) {
	t.Parallel()
	w := wallet.NewHydrator().Hydrate(uuid.New(), uuid.New(), 100, 25, 3, fixedTime, fixedTime)
	if err := w.Release(25, fixedTime); err != nil {
		t.Fatalf("Release(25): %v", err)
	}
	if w.Balance() != 100 {
		t.Errorf("Balance after release = %d, want 100 (no debit)", w.Balance())
	}
	if w.Reserved() != 0 {
		t.Errorf("Reserved after release = %d, want 0", w.Reserved())
	}
	if w.Version() != 4 {
		t.Errorf("Version after release = %d, want 4", w.Version())
	}
}

func TestRelease_RejectsBadInput(t *testing.T) {
	t.Parallel()
	w := wallet.NewHydrator().Hydrate(uuid.New(), uuid.New(), 100, 10, 0, fixedTime, fixedTime)
	if err := w.Release(0, fixedTime); !errors.Is(err, wallet.ErrInvalidAmount) {
		t.Errorf("Release(0): got %v, want ErrInvalidAmount", err)
	}
	if err := w.Release(-1, fixedTime); !errors.Is(err, wallet.ErrInvalidAmount) {
		t.Errorf("Release(-1): got %v, want ErrInvalidAmount", err)
	}
	if err := w.Release(11, fixedTime); !errors.Is(err, wallet.ErrVersionConflict) {
		t.Errorf("Release(11) over reserved=10: got %v, want ErrVersionConflict", err)
	}
}

func TestGrant_HappyPath(t *testing.T) {
	t.Parallel()
	w := wallet.NewHydrator().Hydrate(uuid.New(), uuid.New(), 100, 10, 4, fixedTime, fixedTime)
	if err := w.Grant(500, fixedTime); err != nil {
		t.Fatalf("Grant(500): %v", err)
	}
	if w.Balance() != 600 {
		t.Errorf("Balance after grant = %d, want 600", w.Balance())
	}
	if w.Reserved() != 10 {
		t.Errorf("Reserved after grant = %d, want 10 (unchanged)", w.Reserved())
	}
	if w.Version() != 5 {
		t.Errorf("Version after grant = %d, want 5", w.Version())
	}
}

func TestGrant_RejectsBadAmount(t *testing.T) {
	t.Parallel()
	for _, amt := range []int64{0, -1, -100} {
		w := wallet.NewHydrator().Hydrate(uuid.New(), uuid.New(), 100, 0, 0, fixedTime, fixedTime)
		if err := w.Grant(amt, fixedTime); !errors.Is(err, wallet.ErrInvalidAmount) {
			t.Errorf("Grant(%d): got %v, want ErrInvalidAmount", amt, err)
		}
	}
}

func TestSignedAmount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind wallet.LedgerKind
		in   int64
		want int64
	}{
		{wallet.KindReserve, 100, -100},
		{wallet.KindCommit, 50, -50},
		{wallet.KindRelease, 30, 30},
		{wallet.KindGrant, 1000, 1000},
		{wallet.LedgerKind("unknown"), 7, 7},
	}
	for _, c := range cases {
		t.Run(string(c.kind), func(t *testing.T) {
			if got := wallet.SignedAmount(c.kind, c.in); got != c.want {
				t.Errorf("SignedAmount(%s, %d) = %d, want %d", c.kind, c.in, got, c.want)
			}
		})
	}
}

func TestUpdatedAt_AdvancesOnEveryMutation(t *testing.T) {
	t.Parallel()
	now := fixedTime
	tick := func() time.Time { now = now.Add(time.Second); return now }

	w, err := wallet.New(uuid.New(), fixedTime)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Grant(100, tick()); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if !w.UpdatedAt().Equal(fixedTime.Add(time.Second)) {
		t.Errorf("UpdatedAt after Grant = %v, want %v", w.UpdatedAt(), fixedTime.Add(time.Second))
	}
	if err := w.Reserve(30, tick()); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if !w.UpdatedAt().Equal(fixedTime.Add(2 * time.Second)) {
		t.Errorf("UpdatedAt after Reserve = %v, want %v", w.UpdatedAt(), fixedTime.Add(2*time.Second))
	}
	if err := w.Commit(20, 30, tick()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !w.UpdatedAt().Equal(fixedTime.Add(3 * time.Second)) {
		t.Errorf("UpdatedAt after Commit = %v, want %v", w.UpdatedAt(), fixedTime.Add(3*time.Second))
	}
}
