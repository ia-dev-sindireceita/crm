// Package wallet hosts the WalletAllocator worker for SIN-62881 / Fase 2.5
// C6. The worker subscribes to the subscription.renewed JetStream subject
// produced by the billing renewer (SIN-62879 / C5), looks up the plan's
// monthly_token_quota, and credits the tenant wallet idempotently via
// wallet.MonthlyAllocator.
//
// Idempotency is enforced end-to-end:
//
//   - The JetStream stream's 1h Duplicates window dedups redelivered
//     messages by Nats-Msg-Id = "{subscription_id}:{new_period_start_iso}".
//   - wallet.MonthlyAllocator.AllocateMonthlyQuota writes one
//     token_ledger row per (wallet, idempotency_key) and returns
//     allocated=false on the second call with the same key (UNIQUE
//     (wallet_id, idempotency_key)). The allocator passes the natsMsgID
//     through as the idempotency_key so both layers agree.
//
// Domain code in this package MUST stay free of database/sql, pgx, and
// nats.go imports. Storage lives behind wallet.MonthlyAllocator; the
// plan catalogue lookup lives behind PlanReader; the JetStream
// subscription lives behind EventSubscriber. Concrete implementations
// live in their own adapter packages and are wired in cmd/server.
package wallet

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing"
)

// SubjectSubscriptionRenewed is the JetStream subject the publisher
// (internal/worker/billing) emits to and this consumer reads from. The
// constant is duplicated rather than imported from internal/worker/billing
// to keep the two workers loosely coupled — both must agree on the wire
// shape, but neither needs the other's package as a build-time dep.
const SubjectSubscriptionRenewed = "subscription.renewed"

// Event is the JSON payload decoded from each subscription.renewed
// delivery. The shape mirrors internal/worker/billing.Event verbatim;
// once a consumer ships, additive fields only.
type Event struct {
	SubscriptionID    uuid.UUID `json:"subscription_id"`
	TenantID          uuid.UUID `json:"tenant_id"`
	PlanID            uuid.UUID `json:"plan_id"`
	InvoiceID         uuid.UUID `json:"invoice_id"`
	PreviousPeriodEnd time.Time `json:"previous_period_end"`
	NewPeriodStart    time.Time `json:"new_period_start"`
	NewPeriodEnd      time.Time `json:"new_period_end"`
	AmountCentsBRL    int       `json:"amount_cents_brl"`
	RenewedAt         time.Time `json:"renewed_at"`
}

// Delivery is the small surface the Allocator needs from each JetStream
// message: the raw body to decode, the Nats-Msg-Id header (already used
// by the broker for server-side dedup, and reused as the
// AllocateMonthlyQuota idempotency key for db-side dedup), and the
// per-delivery Ack/Nak controls.
//
// Subscriber-side hygiene rules:
//
//   - Ack is called exactly once on success; double-Ack is a no-op.
//   - Nak signals JetStream to redeliver after delay; the broker still
//     respects MaxDeliver for the durable consumer.
//   - The adapter is responsible for emitting a DLQ message and Acking
//     the original once MaxDeliver is reached — the worker never has to
//     reason about poison pills.
type Delivery interface {
	// Data returns the raw message body. The slice is valid until the
	// delivery is Ack'd / Nak'd.
	Data() []byte
	// MsgID returns the Nats-Msg-Id header. Empty when the publisher
	// did not set it; the worker treats an empty msg-id as fatal-for-
	// this-message so a misconfigured publisher does not silently
	// double-credit.
	MsgID() string
	// Ack confirms successful processing.
	Ack(ctx context.Context) error
	// Nak negatively acknowledges; JetStream will redeliver after the
	// AckWait. Pass a non-zero delay to override (server-side BackOff
	// applies first, this is best-effort).
	Nak(ctx context.Context, delay time.Duration) error
}

// EventSubscriber is the port the Allocator uses to consume from
// JetStream. The implementation owns the durable-consumer name, the
// stream binding, and the AckWait. Subscribe returns a channel of
// deliveries plus a stop function; closing the channel signals that
// the subscription ended (either ctx cancellation or a fatal NATS
// error reported via the returned error channel).
//
// The contract is intentionally small so a fake can drive table-driven
// tests without an embedded NATS server.
type EventSubscriber interface {
	// Subscribe binds a durable JetStream consumer and streams
	// deliveries on the returned channel. Cancelling ctx (or calling
	// the returned stop) drains the subscription. The error channel
	// receives at most one terminal error before the deliveries
	// channel closes.
	Subscribe(ctx context.Context) (<-chan Delivery, <-chan error, error)
}

// PlanReader is the narrow read port the Allocator needs from
// internal/billing's plan catalogue. *billing.Store satisfies it
// structurally; tests substitute a fake.
type PlanReader interface {
	GetPlanByID(ctx context.Context, id uuid.UUID) (billing.Plan, error)
}

// MonthlyAllocator is re-declared here as the worker-local view of
// wallet.MonthlyAllocator. The shapes match exactly so the wireup can
// pass the same value to both ports; defining the interface in the
// worker keeps the dep one-way (worker imports wallet for sentinel
// errors only; the port is owned by the consumer).
type MonthlyAllocator interface {
	AllocateMonthlyQuota(
		ctx context.Context,
		tenantID uuid.UUID,
		periodStart time.Time,
		amount int64,
		idempotencyKey string,
	) (allocated bool, err error)
}
