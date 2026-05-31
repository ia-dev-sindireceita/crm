package llmcustomer_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer"
	"github.com/pericles-luz/crm/internal/inbox"
)

// _ pins the static guarantee that the no-op satisfies the inbox port.
// Lifted into a package-level check so a future port-shape change
// fails the build here instead of at the cmd/server wireup site.
var _ inbox.WalletDebitor = (*llmcustomer.NoopWalletDebitor)(nil)

func TestNoopWalletDebitor_InvokesChargeAndReturnsNil(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	w := llmcustomer.NewNoopWalletDebitor()
	err := w.Debit(context.Background(), uuid.New(), 0, func(context.Context) error {
		called.Store(true)
		return nil
	})
	if err != nil {
		t.Fatalf("Debit returned err = %v, want nil", err)
	}
	if !called.Load() {
		t.Fatal("Debit must invoke the charge callback so the outbound flow exercises wallet bookkeeping")
	}
}

func TestNoopWalletDebitor_PropagatesChargeError(t *testing.T) {
	t.Parallel()
	want := errors.New("carrier send failed")
	w := llmcustomer.NewNoopWalletDebitor()
	err := w.Debit(context.Background(), uuid.New(), 0, func(context.Context) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("Debit returned err = %v, want %v", err, want)
	}
}

func TestNoopWalletDebitor_IgnoresCostButStillInvokesCharge(t *testing.T) {
	t.Parallel()
	// The fake adapter performs no real carrier work, so cost is
	// irrelevant. The WalletDebitor contract still requires charge to
	// run even when cost == 0 (PR4 AC #5); the no-op MUST treat
	// cost > 0 the same way so cmd/server can swap it in for the real
	// debitor without changing the use-case's expectations.
	calls := 0
	w := llmcustomer.NewNoopWalletDebitor()
	err := w.Debit(context.Background(), uuid.New(), 12345, func(context.Context) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("Debit returned err = %v, want nil", err)
	}
	if calls != 1 {
		t.Fatalf("charge called %d times, want exactly 1", calls)
	}
}

func TestNoopWalletDebitor_PropagatesContext(t *testing.T) {
	t.Parallel()
	type ctxKey int
	const k ctxKey = 1
	parent := context.WithValue(context.Background(), k, "trace")
	w := llmcustomer.NewNoopWalletDebitor()
	err := w.Debit(parent, uuid.New(), 0, func(ctx context.Context) error {
		if got := ctx.Value(k); got != "trace" {
			t.Fatalf("context value not propagated; got %v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Debit returned err = %v, want nil", err)
	}
}

func TestNoopWalletDebitor_NilChargeIsNoop(t *testing.T) {
	t.Parallel()
	// The use case always supplies a charge closure, but a defensive
	// nil-guard is cheap and keeps a misconfigured caller from
	// panicking inside the request handler. The contract is
	// "return nil and do nothing" rather than "panic".
	w := llmcustomer.NewNoopWalletDebitor()
	if err := w.Debit(context.Background(), uuid.New(), 0, nil); err != nil {
		t.Fatalf("Debit with nil charge returned err = %v, want nil", err)
	}
}

func TestNoopWalletDebitor_ZeroValueIsUsable(t *testing.T) {
	t.Parallel()
	// The struct holds no state; the zero value must satisfy the
	// contract so callers that construct the adapter via composite
	// literal (no New… helper) still get the documented behaviour.
	var w llmcustomer.NoopWalletDebitor
	called := false
	err := w.Debit(context.Background(), uuid.New(), 0, func(context.Context) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("zero-value Debit returned err = %v, want nil", err)
	}
	if !called {
		t.Fatal("zero-value Debit must invoke charge")
	}
}
