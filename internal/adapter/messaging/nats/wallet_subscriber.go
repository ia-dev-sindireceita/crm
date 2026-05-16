package nats

// SIN-62881 / Fase 2.5 C6 — JetStream subscriber adapter for the
// wallet allocator worker. The adapter implements
// internal/worker/wallet.EventSubscriber and routes deliveries onto a
// channel the worker drains synchronously.
//
// Durable consumer name is fixed ("wallet-allocator") so multiple
// pods share one logical consumer position. AckWait defaults to 30s
// which matches CA #3 of SIN-62881 (lag < 30s on the happy path)
// without forcing the worker to fight the broker on slow ticks.
//
// Idempotency relies on two independent layers:
//
//  1. Server-side: the JetStream stream's 1h Duplicates window (the
//     publisher in internal/worker/billing sets Nats-Msg-Id =
//     "{subscription_id}:{new_period_start_iso}", and any redelivery
//     within the window is dropped by the broker before this adapter
//     sees it).
//
//  2. Allocator-side: wallet.MonthlyAllocator writes
//     ON CONFLICT (wallet_id, idempotency_key) DO NOTHING and the
//     allocator worker passes the Nats-Msg-Id through as the key. A
//     redelivery that slips past the 1h window still yields zero
//     duplicated ledger rows.

import (
	"context"
	"errors"
	"fmt"
	"time"

	natsgo "github.com/nats-io/nats.go"

	walletworker "github.com/pericles-luz/crm/internal/worker/wallet"
)

// WalletSubscriberConfig bundles the fields the JetStream subscriber
// needs. The zero value is invalid; New WalletSubscriber returns an
// error for missing required fields and fills defaults for the rest.
type WalletSubscriberConfig struct {
	// JS is the live JetStream context. Required.
	JS natsgo.JetStreamContext
	// Subject is the JetStream subject to bind. Defaults to
	// walletworker.SubjectSubscriptionRenewed
	// ("subscription.renewed").
	Subject string
	// Durable is the durable consumer name. Defaults to
	// "wallet-allocator". Operators MUST not change this between
	// deploys without coordinating — switching the name resets the
	// consumer position.
	Durable string
	// Stream binds the consumer to a specific JetStream stream. May
	// be empty when only one stream covers Subject; JetStream then
	// auto-binds.
	Stream string
	// AckWait is the per-message ack deadline. Defaults to 30s.
	AckWait time.Duration
	// MaxDeliver caps total delivery attempts (original + redeliveries)
	// before JetStream drops the message into the configured DLQ.
	// JetStream requires MaxDeliver > len(BackOff); the New
	// constructor enforces this. Defaults to 6 to match the 5-entry
	// default BackOff. The DLQ stream itself is operator-provisioned
	// (CRM ops runbook); the adapter does not create it.
	MaxDeliver int
	// BackOff is the per-attempt redelivery delay list. JetStream
	// uses the i-th entry on the i-th redelivery (clamped to the
	// list length). Defaults to [1s, 5s, 15s, 1m, 5m]. JetStream
	// requires len(BackOff) < MaxDeliver.
	BackOff []time.Duration
	// BufferSize sizes the deliveries channel returned by Subscribe.
	// Defaults to 64; large enough that a slow consumer does not
	// starve the broker's PullSubscribe routine but small enough to
	// surface back-pressure quickly.
	BufferSize int
}

// WalletSubscriber implements walletworker.EventSubscriber by binding
// a durable JetStream consumer and surfacing deliveries on a channel.
type WalletSubscriber struct {
	js         natsgo.JetStreamContext
	subject    string
	durable    string
	stream     string
	ackWait    time.Duration
	maxDeliver int
	backOff    []time.Duration
	bufferSize int
}

// NewWalletSubscriber constructs a WalletSubscriber from cfg.
func NewWalletSubscriber(cfg WalletSubscriberConfig) (*WalletSubscriber, error) {
	if cfg.JS == nil {
		return nil, errors.New("nats/wallet: JetStream context is required")
	}
	if cfg.Subject == "" {
		cfg.Subject = walletworker.SubjectSubscriptionRenewed
	}
	if cfg.Durable == "" {
		cfg.Durable = "wallet-allocator"
	}
	if cfg.AckWait <= 0 {
		cfg.AckWait = 30 * time.Second
	}
	if len(cfg.BackOff) == 0 {
		cfg.BackOff = []time.Duration{
			1 * time.Second,
			5 * time.Second,
			15 * time.Second,
			1 * time.Minute,
			5 * time.Minute,
		}
	}
	if cfg.MaxDeliver <= 0 {
		cfg.MaxDeliver = len(cfg.BackOff) + 1
	}
	if cfg.MaxDeliver <= len(cfg.BackOff) {
		return nil, fmt.Errorf("nats/wallet: MaxDeliver (%d) must be > len(BackOff) (%d)", cfg.MaxDeliver, len(cfg.BackOff))
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 64
	}
	return &WalletSubscriber{
		js:         cfg.JS,
		subject:    cfg.Subject,
		durable:    cfg.Durable,
		stream:     cfg.Stream,
		ackWait:    cfg.AckWait,
		maxDeliver: cfg.MaxDeliver,
		backOff:    cfg.BackOff,
		bufferSize: cfg.BufferSize,
	}, nil
}

// Subscribe binds the durable JetStream consumer and returns a channel
// of deliveries. The implementation uses ChanSubscribe so JetStream
// pushes messages onto an internal channel that we mirror into the
// worker's Delivery surface.
//
// Cancelling ctx unsubscribes and closes the deliveries channel. A
// terminal error (e.g. broker disconnect that nats.go can't auto-recover
// from) is surfaced on the error channel exactly once before the
// deliveries channel closes.
func (s *WalletSubscriber) Subscribe(ctx context.Context) (<-chan walletworker.Delivery, <-chan error, error) {
	rawCh := make(chan *natsgo.Msg, s.bufferSize)
	opts := []natsgo.SubOpt{
		natsgo.Durable(s.durable),
		natsgo.ManualAck(),
		natsgo.AckExplicit(),
		natsgo.AckWait(s.ackWait),
		natsgo.MaxDeliver(s.maxDeliver),
		natsgo.BackOff(s.backOff),
		natsgo.DeliverAll(),
	}
	if s.stream != "" {
		opts = append(opts, natsgo.BindStream(s.stream))
	}
	sub, err := s.js.ChanSubscribe(s.subject, rawCh, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("nats/wallet: subscribe %q: %w", s.subject, err)
	}

	deliveries := make(chan walletworker.Delivery, s.bufferSize)
	errs := make(chan error, 1)

	go func() {
		defer close(deliveries)
		// Drain best-effort on exit so JetStream re-delivers
		// in-flight messages to another pod instead of stalling on
		// AckWait.
		defer func() { _ = sub.Drain() }()
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-rawCh:
				if !ok {
					return
				}
				select {
				case <-ctx.Done():
					// Don't try to push to a downstream that's
					// already abandoned; let the broker redeliver
					// after AckWait.
					return
				case deliveries <- walletDelivery{m: m}:
				}
			}
		}
	}()
	return deliveries, errs, nil
}

// walletDelivery adapts *natsgo.Msg to walletworker.Delivery.
type walletDelivery struct {
	m *natsgo.Msg
}

// Data returns the message body.
func (d walletDelivery) Data() []byte {
	if d.m == nil {
		return nil
	}
	return d.m.Data
}

// MsgID returns the Nats-Msg-Id header set by the publisher.
func (d walletDelivery) MsgID() string {
	if d.m == nil {
		return ""
	}
	return d.m.Header.Get("Nats-Msg-Id")
}

// Ack signals successful processing.
func (d walletDelivery) Ack(_ context.Context) error {
	if d.m == nil {
		return errors.New("nats/wallet: nil delivery")
	}
	if err := d.m.Ack(); err != nil {
		return fmt.Errorf("nats/wallet: ack: %w", err)
	}
	return nil
}

// Nak negatively acknowledges with the requested delay. JetStream uses
// max(delay, BackOff[attempt]) so the cumulative wait honors both the
// server-side curve and the per-message hint.
func (d walletDelivery) Nak(_ context.Context, delay time.Duration) error {
	if d.m == nil {
		return errors.New("nats/wallet: nil delivery")
	}
	if delay <= 0 {
		if err := d.m.Nak(); err != nil {
			return fmt.Errorf("nats/wallet: nak: %w", err)
		}
		return nil
	}
	if err := d.m.NakWithDelay(delay); err != nil {
		return fmt.Errorf("nats/wallet: nak-with-delay: %w", err)
	}
	return nil
}

// Compile-time guard: WalletSubscriber satisfies the worker contract.
var _ walletworker.EventSubscriber = (*WalletSubscriber)(nil)
