package wallet_test

import (
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
)

// TestGrant_RejectsOverflow encodes finding #4 from SIN-62748: an
// adversarial Grant amount that would push balance past math.MaxInt64
// MUST be rejected at the domain with ErrInvalidAmount, not allowed to
// wrap and surface later as an opaque database CHECK violation.
func TestGrant_RejectsOverflow(t *testing.T) {
	t.Parallel()

	// Start near the int64 ceiling so the next Grant can wrap.
	w := wallet.Hydrate(uuid.New(), uuid.New(), math.MaxInt64-10, 0, 0, fixedTime, fixedTime)

	// 11 is one past the headroom: balance(MaxInt64-10) + 11 overflows.
	if err := w.Grant(11, fixedTime); !errors.Is(err, wallet.ErrInvalidAmount) {
		t.Errorf("Grant(11) at MaxInt64-10 headroom: got %v, want ErrInvalidAmount", err)
	}
	if w.Balance() != math.MaxInt64-10 || w.Version() != 0 {
		t.Errorf("overflow-rejected Grant mutated state: balance=%d version=%d", w.Balance(), w.Version())
	}

	// MaxInt64 itself against a non-zero balance also wraps.
	w2 := wallet.Hydrate(uuid.New(), uuid.New(), 1, 0, 0, fixedTime, fixedTime)
	if err := w2.Grant(math.MaxInt64, fixedTime); !errors.Is(err, wallet.ErrInvalidAmount) {
		t.Errorf("Grant(MaxInt64) on balance=1: got %v, want ErrInvalidAmount", err)
	}
	if w2.Balance() != 1 || w2.Version() != 0 {
		t.Errorf("overflow-rejected Grant mutated state: balance=%d version=%d", w2.Balance(), w2.Version())
	}
}

// TestGrant_ExactHeadroomSucceeds is the boundary case: a Grant that
// brings balance to exactly math.MaxInt64 is legal because the cap is
// strict-greater-than (no wrap).
func TestGrant_ExactHeadroomSucceeds(t *testing.T) {
	t.Parallel()
	w := wallet.Hydrate(uuid.New(), uuid.New(), math.MaxInt64-10, 0, 0, fixedTime, fixedTime)
	if err := w.Grant(10, fixedTime); err != nil {
		t.Fatalf("Grant(10) at MaxInt64-10 headroom: got %v, want nil", err)
	}
	if w.Balance() != math.MaxInt64 {
		t.Errorf("Balance after exact-headroom Grant = %d, want MaxInt64", w.Balance())
	}
}
