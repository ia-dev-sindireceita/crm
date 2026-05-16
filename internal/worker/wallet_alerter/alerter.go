// Package wallet_alerter consumes the `wallet.balance.depleted`
// JetStream subject (published by the wallet debit path / SIN-62903 W2C
// and wave-3 SIN-62905 W3C) and emits a single Slack message to the
// operator `#alerts` channel for every depletion event.
//
// Hexagonal layout:
//
//   - Inputs (Delivery, Subscriber) are narrow interfaces. The NATS
//     adapter (internal/adapter/messaging/nats) implements them; tests
//     use in-process fakes or the embedded JetStream server.
//   - Outputs (Notifier) point at internal/adapter/notify/slack — the
//     existing webhook adapter whose empty-URL branch turns Notify into
//     a no-op so an operator can opt the alerter in via env without
//     touching code (AC #3 of SIN-62905: "SLACK_ALERTS_WEBHOOK_URL
//     ausente → worker loga warning e segue").
//
// Security posture (LGPD + ADR 0073):
//
//   - The webhook URL is treated as a secret. It is never logged,
//     never echoed in error wrappers, never serialised into a JSON
//     body the caller controls. Only the response status and our own
//     constants leak.
//   - The payload carries tenant_id and a debit amount. tenant_id is
//     opaque (uuid-shaped); we still avoid logging the formatted Slack
//     body because policy_scope can encode tenant-scoped strings.
//   - Idempotency is enforced on (tenant_id, occurred_at) with a 1h
//     TTL. JetStream redeliveries — and accidental re-emits from the
//     producer — collapse to a single Slack POST. The TTL matches the
//     JetStream `Duplicates` window used elsewhere in the codebase so
//     we do not need an external store for the dev VPS phase.
//
// Failure-mode contract:
//
//   - Malformed JSON or a missing required field is poison: the worker
//     logs a Warn and ACKs the delivery so JetStream does not redeliver
//     a payload that will never decode.
//   - A Notifier error is transient: the worker returns the error to
//     the SDK adapter so JetStream redelivers after AckWait. The dedup
//     cache is updated ONLY on a successful Notify so a redelivery has
//     a chance to retry.
package wallet_alerter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Subject is the JetStream subject this worker subscribes to. Producers
// (the wallet debit path) MUST publish here when a debit moves a tenant
// wallet balance to zero.
const Subject = "wallet.balance.depleted"

// QueueName is the JetStream queue group. A single deploy may run
// multiple replicas; one delivery is handed to exactly one replica.
const QueueName = "wallet-alerter"

// DurableName is the JetStream durable consumer name. Reuse across
// restarts so the cursor survives a crash.
const DurableName = "wallet-alerter-v1"

// StreamName is the JetStream stream the worker expects to read from.
// EnsureStream is the caller's responsibility (the cmd entrypoint owns
// stream lifecycle, same as cmd/mediascan-worker).
const StreamName = "WALLET"

// DefaultDedupTTL is the in-memory dedup window. Matches the JetStream
// `Duplicates` window the publisher relies on; events arriving inside
// this window with the same (tenant_id, occurred_at) collapse to a
// single Slack POST.
const DefaultDedupTTL = time.Hour

// Event is the JSON payload carried on Subject. Field tags match the
// snake_case wire format committed in SIN-62903's spec. Extra fields
// are tolerated by the JSON decoder and dropped — the producer is free
// to add metadata (e.g. correlation_id) without bumping the consumer.
type Event struct {
	TenantID         string    `json:"tenant_id"`
	PolicyScope      string    `json:"policy_scope"`
	LastChargeTokens int64     `json:"last_charge_tokens"`
	OccurredAt       time.Time `json:"occurred_at"`
}

// Notifier is the narrow outbound port. The Slack webhook adapter
// (internal/adapter/notify/slack.Notifier) satisfies it directly; tests
// inject a recording fake.
type Notifier interface {
	Notify(ctx context.Context, msg string) error
}

// Delivery is one redeliverable JetStream message handed to Handle.
// Ack is idempotent at the SDK layer (calling it twice is a no-op).
type Delivery interface {
	Data() []byte
	Ack(ctx context.Context) error
}

// Clock allows tests to drive the dedup TTL deterministically. The
// production wiring uses a wall-clock; the unit tests use a manual
// clock so a 1h TTL can be exercised without sleeping.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production wall-clock implementation.
type SystemClock struct{}

// Now returns time.Now().UTC(). UTC is forced so the formatted occurred_at
// in the Slack message is stable across deploy timezones.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// Alerter is the unit-testable core: it owns the decoder, dedup cache,
// formatter and Notifier dispatch. The Run helper below wires it into
// a JetStream subscription, but Alerter itself does not import the SDK
// adapter so the unit tests stay fast.
type Alerter struct {
	notifier Notifier
	dedup    *Dedup
	logger   *slog.Logger
}

// New constructs an Alerter. notifier and logger are required; the
// dedup cache defaults to DefaultDedupTTL when ttl <= 0. Pass an
// already-built Notifier — when the adapter is the Slack webhook with
// an empty URL it degrades to a no-op (AC #3) without needing a flag
// here.
func New(notifier Notifier, logger *slog.Logger, clock Clock, ttl time.Duration) (*Alerter, error) {
	if notifier == nil {
		return nil, errors.New("wallet_alerter: Notifier is required")
	}
	if logger == nil {
		return nil, errors.New("wallet_alerter: logger is required")
	}
	if clock == nil {
		clock = SystemClock{}
	}
	if ttl <= 0 {
		ttl = DefaultDedupTTL
	}
	return &Alerter{
		notifier: notifier,
		dedup:    NewDedup(ttl, clock),
		logger:   logger,
	}, nil
}

// Handle is the per-delivery callback. It decodes the JSON body,
// validates the required fields, dedups against (tenant_id, occurred_at),
// formats the Slack body, and dispatches via the Notifier.
//
// Return semantics:
//
//   - nil  → ack the delivery (success, poison, or dedup hit)
//   - err  → leave unacked so JetStream redelivers after AckWait
func (a *Alerter) Handle(ctx context.Context, d Delivery) error {
	if d == nil {
		return errors.New("wallet_alerter: nil delivery")
	}
	body := d.Data()
	if len(body) == 0 {
		a.logger.Warn("wallet_alerter: empty payload, dropping")
		return d.Ack(ctx)
	}
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		// Poison payload: log and ack, do not redeliver a body that will
		// never decode. Raw bytes are intentionally omitted from the log
		// (may contain tenant-scoped material).
		a.logger.Warn("wallet_alerter: malformed payload",
			"err", err.Error(),
			"bytes", len(body),
		)
		return d.Ack(ctx)
	}
	if err := ev.validate(); err != nil {
		a.logger.Warn("wallet_alerter: invalid payload",
			"err", err.Error(),
			"tenant_id", ev.TenantID,
		)
		return d.Ack(ctx)
	}
	if a.dedup.Seen(ev.TenantID, ev.OccurredAt) {
		a.logger.Info("wallet_alerter: duplicate event suppressed",
			"tenant_id", ev.TenantID,
			"occurred_at", ev.OccurredAt.UTC().Format(time.RFC3339Nano),
		)
		return d.Ack(ctx)
	}
	msg := FormatMessage(ev)
	if err := a.notifier.Notify(ctx, msg); err != nil {
		// Transient: do not record the event in the dedup cache so a
		// redelivery can retry.
		a.logger.Error("wallet_alerter: slack notify failed",
			"err", err.Error(),
			"tenant_id", ev.TenantID,
		)
		return fmt.Errorf("wallet_alerter: notify: %w", err)
	}
	a.dedup.Record(ev.TenantID, ev.OccurredAt)
	a.logger.Info("wallet_alerter: alert dispatched",
		"tenant_id", ev.TenantID,
		"policy_scope", ev.PolicyScope,
		"last_charge_tokens", ev.LastChargeTokens,
		"occurred_at", ev.OccurredAt.UTC().Format(time.RFC3339Nano),
	)
	return d.Ack(ctx)
}

// validate enforces the required-field contract. Missing tenant_id /
// occurred_at is treated as poison; a zero last_charge_tokens is
// tolerated because the publisher's only AC is "balance hit zero" and
// the last debit may legitimately have been a no-op refund.
func (e Event) validate() error {
	if e.TenantID == "" {
		return errors.New("tenant_id is required")
	}
	if e.OccurredAt.IsZero() {
		return errors.New("occurred_at is required")
	}
	return nil
}

// FormatMessage renders the human-readable Slack body. The format
// matches the SIN-62905 description verbatim so the AC can be diffed
// against the test golden.
//
// PII discipline: the message contains tenant_id (opaque uuid) and
// policy_scope (tenant-scoped string). Adapters MUST NOT log the
// formatted body — only structural metadata.
func FormatMessage(e Event) string {
	return fmt.Sprintf(
		":warning: Wallet zerada em tenant `%s` (escopo `%s`). Último débito: %d tokens em %s.",
		e.TenantID,
		e.PolicyScope,
		e.LastChargeTokens,
		e.OccurredAt.UTC().Format(time.RFC3339),
	)
}
