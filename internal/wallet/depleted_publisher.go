package wallet

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// BalanceDepletedEvent is the domain payload emitted when a debit
// brings a tenant wallet balance to zero. It is the trigger captured by
// the wallet-alerter worker (internal/worker/wallet_alerter) — the
// adapter translates it into the snake_case JSON wire format the worker
// decodes.
//
// PolicyScope is forward-looking: Fase 3 wallets are tenant-scoped
// (single bucket per tenant), so the only producer-side value today is
// "tenant:default". The field is on the contract so a future per-policy
// allocator can fire scoped depletion events without breaking the wire
// format.
//
// LastChargeTokens is the amount of the debit that produced the zero
// balance (the Commit's actualAmount). It is reported in tokens for
// parity with the wallet aggregate's int64 balance.
type BalanceDepletedEvent struct {
	TenantID         uuid.UUID
	PolicyScope      string
	LastChargeTokens int64
	OccurredAt       time.Time
}

// BalanceDepletedPublisher is the domain port the wallet use-case
// invokes after a successful Commit that brings the balance to zero.
// The contract is best-effort:
//
//   - The wallet transaction has already committed; a publish failure
//     MUST NOT roll the debit back.
//   - The implementation owns transport-level retries, broker dedup,
//     and any outbox journaling. The use-case treats the call as
//     fire-and-forget plus log-on-error.
//
// The NATS-backed implementation lives at
// internal/adapter/messaging/nats/wallet_depleted_publisher.go. Tests
// inject a recording fake; production wiring at cmd/server passes a
// real adapter. Callers that have no broker configured pass
// NoOpBalanceDepletedPublisher{}, the default the Service uses when no
// publisher option is supplied.
type BalanceDepletedPublisher interface {
	PublishBalanceDepleted(ctx context.Context, evt BalanceDepletedEvent) error
}

// NoOpBalanceDepletedPublisher is the default publisher used when no
// real adapter is wired. It returns nil unconditionally so the
// degraded-mode path (no NATS) keeps debits flowing without surfacing
// a "publisher missing" error to every caller.
type NoOpBalanceDepletedPublisher struct{}

// PublishBalanceDepleted satisfies BalanceDepletedPublisher with a
// no-op success. The Service still inspects the error so a future
// non-nil implementation can opt into logging.
func (NoOpBalanceDepletedPublisher) PublishBalanceDepleted(_ context.Context, _ BalanceDepletedEvent) error {
	return nil
}
