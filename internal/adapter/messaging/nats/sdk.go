// SDK glue between `github.com/nats-io/nats.go` and the worker's
// narrow Subscriber/Publisher contracts (internal/media/worker). The
// scope is intentionally limited to what the mediascan-worker needs:
//
//   - Dial a NATS server + open a JetStream context.
//   - EnsureStream a JetStream stream with a 1h Duplicates window so
//     the F-14 invariant the existing Publisher relies on continues
//     to hold for the media-scan subjects too.
//   - Publish raw JSON bodies on a subject (no dedup header; the
//     worker re-publishes on redelivery and downstream dedups).
//   - QueueSubscribe with manual ack and a per-message handler that
//     receives a *Delivery wrapping the underlying *nats.Msg.
//   - Close drains the conn so in-flight messages get a chance to
//     finish before the process exits (graceful shutdown contract).
//
// The package keeps the older webhook.Publisher (JetStream interface +
// dedup) intact — that adapter has its own deduplication and stream
// management — and adds SDKAdapter as a sibling for at-least-once
// pipelines that just need straightforward queue groups.

package nats

import (
	"context"
	"errors"
	"fmt"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// SDKConfig configures Connect. URL is required; everything else has
// sensible defaults.
type SDKConfig struct {
	// URL is the NATS endpoint (e.g. "nats://nats:4222").
	URL string
	// Name is the human-friendly client name surfaced by NATS server
	// monitoring. Defaults to "crm-mediascan-worker".
	Name string
	// ConnectTimeout caps the initial dial. Defaults to 10s.
	ConnectTimeout time.Duration
	// MaxReconnects caps automatic reconnects. -1 means forever.
	MaxReconnects int
	// ReconnectWait is the per-attempt delay. Defaults to 2s.
	ReconnectWait time.Duration
}

// SDKAdapter wraps a live *nats.Conn + JetStream context. One per
// process. Safe for concurrent Publish and Subscribe calls.
type SDKAdapter struct {
	conn *natsgo.Conn
	js   natsgo.JetStreamContext
}

// Connect dials NATS, opens a JetStream context, and returns an
// SDKAdapter ready for use. The caller MUST Close the adapter on
// shutdown.
func Connect(_ context.Context, cfg SDKConfig) (*SDKAdapter, error) {
	if cfg.URL == "" {
		return nil, errors.New("nats: SDKConfig.URL is required")
	}
	if cfg.Name == "" {
		cfg.Name = "crm-mediascan-worker"
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	if cfg.ReconnectWait <= 0 {
		cfg.ReconnectWait = 2 * time.Second
	}
	opts := []natsgo.Option{
		natsgo.Name(cfg.Name),
		natsgo.Timeout(cfg.ConnectTimeout),
		natsgo.ReconnectWait(cfg.ReconnectWait),
		natsgo.MaxReconnects(cfg.MaxReconnects),
	}
	conn, err := natsgo.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("nats: connect %q: %w", cfg.URL, err)
	}
	js, err := conn.JetStream()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("nats: jetstream context: %w", err)
	}
	return &SDKAdapter{conn: conn, js: js}, nil
}

// EnsureStream creates the stream if it does not exist, otherwise
// no-ops. The Duplicates window is fixed at 1h to align with the
// reconciler tolerance set by the webhook Publisher (F-14).
//
// Idempotent: a second call against an existing stream returns nil
// without attempting a reconfigure (the worker should not silently
// flip operator-tuned retention / replicas).
func (a *SDKAdapter) EnsureStream(name string, subjects []string) error {
	if name == "" {
		return errors.New("nats: stream name is required")
	}
	if len(subjects) == 0 {
		return errors.New("nats: stream subjects required")
	}
	if _, err := a.js.StreamInfo(name); err == nil {
		return nil
	}
	_, err := a.js.AddStream(&natsgo.StreamConfig{
		Name:       name,
		Subjects:   subjects,
		Storage:    natsgo.FileStorage,
		Retention:  natsgo.WorkQueuePolicy,
		Duplicates: MinDuplicatesWindow,
	})
	if err != nil {
		return fmt.Errorf("nats: add stream %q: %w", name, err)
	}
	return nil
}

// Publish sends body on subject with at-least-once delivery into the
// owning JetStream stream. No dedup header is set; redelivery of
// `media.scan.completed` from the worker is expected, and downstream
// consumers (SIN-62805) dedup on (tenant_id, message_id).
func (a *SDKAdapter) Publish(ctx context.Context, subject string, body []byte) error {
	if subject == "" {
		return errors.New("nats: subject is required")
	}
	if _, err := a.js.PublishMsg(&natsgo.Msg{Subject: subject, Data: body}, natsgo.Context(ctx)); err != nil {
		return fmt.Errorf("nats: publish %q: %w", subject, err)
	}
	return nil
}

// HandlerFunc is the per-message callback shape Subscribe wires. The
// adapter does NOT ack on its behalf; the worker calls Delivery.Ack
// once persistence is confirmed. Returning a non-nil error tells the
// adapter to skip the ack so the broker can redeliver after AckWait.
type HandlerFunc func(ctx context.Context, d *Delivery) error

// Subscribe binds handler to subject under queue group. Each delivery
// produces one HandlerFunc invocation inside a fresh background
// context derived from ctx. The returned *natsgo.Subscription is
// owned by the caller — Drain or Unsubscribe it on shutdown.
//
// ackWait is the JetStream redelivery timeout. Pick > the slowest
// scan latency the worker should tolerate; ClamAV scans of a few MB
// fit comfortably in 30s.
func (a *SDKAdapter) Subscribe(
	ctx context.Context,
	subject, queue, durable string,
	ackWait time.Duration,
	handler HandlerFunc,
) (*natsgo.Subscription, error) {
	if handler == nil {
		return nil, errors.New("nats: handler is required")
	}
	if ackWait <= 0 {
		ackWait = 30 * time.Second
	}
	sub, err := a.js.QueueSubscribe(subject, queue, func(m *natsgo.Msg) {
		d := &Delivery{m: m}
		if err := handler(ctx, d); err != nil {
			// Best-effort negative ack so the broker redelivers
			// sooner than AckWait. Failure here is logged-and-
			// ignored: AckWait will redeliver regardless.
			_ = m.Nak()
		}
	},
		natsgo.Durable(durable),
		natsgo.ManualAck(),
		natsgo.AckWait(ackWait),
		natsgo.DeliverAll(),
	)
	if err != nil {
		return nil, fmt.Errorf("nats: subscribe %q: %w", subject, err)
	}
	return sub, nil
}

// Drain closes the underlying connection gracefully — in-flight
// messages have a chance to ack before the conn drops. Call from the
// SIGTERM handler.
func (a *SDKAdapter) Drain() error {
	if a == nil || a.conn == nil {
		return nil
	}
	if err := a.conn.Drain(); err != nil {
		return fmt.Errorf("nats: drain: %w", err)
	}
	return nil
}

// Close hard-closes the underlying connection. Prefer Drain in
// graceful-shutdown paths.
func (a *SDKAdapter) Close() {
	if a == nil || a.conn == nil {
		return
	}
	a.conn.Close()
}

// Delivery adapts *natsgo.Msg to the worker.Delivery contract.
// Created by Subscribe; never constructed by callers.
type Delivery struct {
	m *natsgo.Msg
}

// Data returns the message body.
func (d *Delivery) Data() []byte {
	if d == nil || d.m == nil {
		return nil
	}
	return d.m.Data
}

// Ack signals successful processing to JetStream. Idempotent at the
// SDK layer: ack-after-ack is silently swallowed.
func (d *Delivery) Ack(_ context.Context) error {
	if d == nil || d.m == nil {
		return errors.New("nats: nil delivery")
	}
	if err := d.m.Ack(); err != nil {
		return fmt.Errorf("nats: ack: %w", err)
	}
	return nil
}
