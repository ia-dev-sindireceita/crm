package llmcustomer

import (
	"context"

	"github.com/google/uuid"
)

// NoopWalletDebitor satisfies inbox.WalletDebitor for the fake-customer
// channel. The fake adapter never reaches a real carrier, so there is
// no token spend to reserve; the no-op runs the supplied charge
// callback exactly once and returns its error verbatim.
//
// Wired only when INBOX_CHANNEL_PROVIDER=llmcustomer (see
// cmd/server/inbox_channel_provider_wire.go). Production deploys never
// see this adapter — the boot gate refuses fake-customer activation on
// production-tier APP_ENV.
//
// Concurrency: the zero value is safe for concurrent use. The struct
// holds no state intentionally; if a future iteration needs charge
// accounting it should grow a dedicated type instead of bolting state
// onto the no-op contract.
type NoopWalletDebitor struct{}

// NewNoopWalletDebitor returns a NoopWalletDebitor. A constructor
// keeps composition-root wiring uniform across adapter packages
// (cmd/server reads each adapter via New… so a future change to the
// no-op's shape — e.g. an opt-in metric counter — has one obvious
// extension point).
func NewNoopWalletDebitor() *NoopWalletDebitor { return &NoopWalletDebitor{} }

// Debit honours the WalletDebitor contract: invoke the charge callback
// and return its error. cost is ignored because the fake adapter
// performs no carrier work that consumes wallet tokens; the inbox use
// case still calls Debit on every send so the outbound flow exercises
// the wallet bookkeeping path uniformly (PR4 AC #5 — same invariant
// the real WalletDebitor must honour, see
// internal/inbox/port_outbound.go).
//
// A nil charge would violate the contract — the inbox use case always
// supplies the carrier-send closure — so we defensively return without
// error in that case rather than panicking and crashing the request
// handler.
func (NoopWalletDebitor) Debit(ctx context.Context, _ uuid.UUID, _ int64, charge func(ctx context.Context) error) error {
	if charge == nil {
		return nil
	}
	return charge(ctx)
}
