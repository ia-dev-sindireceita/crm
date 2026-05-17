package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/worker/wallet_alerter"
)

// walletDepletedPublishTarget is the JetStream surface the
// WalletDepletedPublisher writes to. The SDKAdapter satisfies it
// directly via PublishMsgID; tests inject a recording fake.
type walletDepletedPublishTarget interface {
	PublishMsgID(ctx context.Context, subject, msgID string, body []byte) error
}

// WalletDepletedPublisher implements wallet.BalanceDepletedPublisher by
// serialising the domain event into the snake_case JSON envelope the
// wallet-alerter worker (internal/worker/wallet_alerter) decodes, then
// publishing it to wallet_alerter.Subject on the WALLET JetStream
// stream.
//
// Subject + StreamName are imported from the consumer package so the
// producer cannot drift from the contract the worker pins. Option A
// of SIN-62934: subject is "wallet.balance.depleted" (no .v1 suffix);
// ADR 0043 is updated to match (was previously documented with a
// .v1 suffix that never shipped — the consumer was merged with the
// unsuffixed subject in #163).
//
// Dedup: the Nats-Msg-Id header is set to "<tenant_id>:<occurred_at>"
// so identical events published twice within the stream's Duplicates
// window (1h, enforced by EnsureStream) collapse to a single delivery.
// This complements the worker's in-memory dedup cache, which catches
// duplicates that race past the broker window or arrive after a worker
// restart. Together they provide the at-most-once behaviour the
// operator runbook documents.
type WalletDepletedPublisher struct {
	target walletDepletedPublishTarget
}

// NewWalletDepletedPublisher constructs the publisher. target is the
// already-connected JetStream surface (an *SDKAdapter in production);
// nil is rejected so a misconfigured boot fails fast.
func NewWalletDepletedPublisher(target walletDepletedPublishTarget) (*WalletDepletedPublisher, error) {
	if target == nil {
		return nil, errors.New("nats: WalletDepletedPublisher target is required")
	}
	return &WalletDepletedPublisher{target: target}, nil
}

// PublishBalanceDepleted satisfies wallet.BalanceDepletedPublisher.
//
// Wire envelope (matches wallet_alerter.Event JSON tags verbatim — the
// consumer is the source of truth):
//
//	{
//	  "tenant_id":          "<uuid>",
//	  "policy_scope":       "tenant:default",
//	  "last_charge_tokens": <int>,
//	  "occurred_at":        "<RFC3339Nano UTC>"
//	}
//
// The event's TenantID is the uuid bytes; the consumer treats
// tenant_id as opaque (string) so the wire shape stays human-readable
// for ops grepping JetStream backlogs.
func (p *WalletDepletedPublisher) PublishBalanceDepleted(ctx context.Context, evt wallet.BalanceDepletedEvent) error {
	wire := wallet_alerter.Event{
		TenantID:         evt.TenantID.String(),
		PolicyScope:      evt.PolicyScope,
		LastChargeTokens: evt.LastChargeTokens,
		OccurredAt:       evt.OccurredAt.UTC(),
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return fmt.Errorf("nats: marshal balance.depleted: %w", err)
	}
	msgID := wire.TenantID + ":" + strconv.FormatInt(wire.OccurredAt.UnixNano(), 10)
	if err := p.target.PublishMsgID(ctx, wallet_alerter.Subject, msgID, body); err != nil {
		return fmt.Errorf("nats: publish balance.depleted: %w", err)
	}
	return nil
}

// Compile-time fence — keep the port and adapter in sync at build time.
var _ wallet.BalanceDepletedPublisher = (*WalletDepletedPublisher)(nil)
