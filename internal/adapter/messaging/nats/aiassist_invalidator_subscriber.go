package nats

// SIN-62908 / Fase 3 W4D — JetStream subscriber adapter for the
// AISummary cache invalidator worker. Mirrors the wallet subscriber
// (SIN-62881) so cmd/server can wire both with the same pattern.
//
// Durable consumer name is fixed ("aiassist-invalidator") so multiple
// pods share one logical consumer position. AckWait defaults to 30s.
//
// Idempotency: Invalidate is idempotent at the domain level, so a
// JetStream redelivery never produces a duplicate side-effect. The
// adapter still passes Nats-Msg-Id through to the worker for log
// correlation.

import (
	"context"
	"errors"
	"fmt"
	"time"

	natsgo "github.com/nats-io/nats.go"

	aiassistinvalidator "github.com/pericles-luz/crm/internal/worker/aiassist_invalidator"
)

// AIAssistInvalidatorSubscriberConfig bundles the fields the
// subscriber needs.
type AIAssistInvalidatorSubscriberConfig struct {
	// JS is the live JetStream context. Required.
	JS natsgo.JetStreamContext
	// Subject is the JetStream subject to bind. Defaults to
	// aiassistinvalidator.SubjectMessageCreated ("message.created").
	Subject string
	// Durable is the durable consumer name. Defaults to
	// "aiassist-invalidator". Operators MUST not change this between
	// deploys without coordinating — switching the name resets the
	// consumer position.
	Durable string
	// Stream binds the consumer to a specific JetStream stream. May
	// be empty when only one stream covers Subject.
	Stream string
	// AckWait is the per-message ack deadline. Defaults to 30s.
	AckWait time.Duration
	// MaxDeliver caps total delivery attempts before JetStream drops
	// the message into the configured DLQ. JetStream requires
	// MaxDeliver > len(BackOff); the New constructor enforces this.
	// Defaults to len(BackOff)+1.
	MaxDeliver int
	// BackOff is the per-attempt redelivery delay list. Defaults to
	// [1s, 5s, 15s, 1m, 5m].
	BackOff []time.Duration
	// BufferSize sizes the deliveries channel. Defaults to 64.
	BufferSize int
}

// AIAssistInvalidatorSubscriber implements aiassistinvalidator.EventSubscriber
// by binding a durable JetStream consumer.
type AIAssistInvalidatorSubscriber struct {
	js         natsgo.JetStreamContext
	subject    string
	durable    string
	stream     string
	ackWait    time.Duration
	maxDeliver int
	backOff    []time.Duration
	bufferSize int
}

// NewAIAssistInvalidatorSubscriber constructs the subscriber.
func NewAIAssistInvalidatorSubscriber(cfg AIAssistInvalidatorSubscriberConfig) (*AIAssistInvalidatorSubscriber, error) {
	if cfg.JS == nil {
		return nil, errors.New("nats/aiassist: JetStream context is required")
	}
	if cfg.Subject == "" {
		cfg.Subject = aiassistinvalidator.SubjectMessageCreated
	}
	if cfg.Durable == "" {
		cfg.Durable = "aiassist-invalidator"
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
		return nil, fmt.Errorf("nats/aiassist: MaxDeliver (%d) must be > len(BackOff) (%d)", cfg.MaxDeliver, len(cfg.BackOff))
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 64
	}
	return &AIAssistInvalidatorSubscriber{
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
// of deliveries. Cancelling ctx unsubscribes and closes the deliveries
// channel.
func (s *AIAssistInvalidatorSubscriber) Subscribe(ctx context.Context) (<-chan aiassistinvalidator.Delivery, <-chan error, error) {
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
		return nil, nil, fmt.Errorf("nats/aiassist: subscribe %q: %w", s.subject, err)
	}

	deliveries := make(chan aiassistinvalidator.Delivery, s.bufferSize)
	errs := make(chan error, 1)

	go func() {
		defer close(deliveries)
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
					return
				case deliveries <- aiassistDelivery{m: m}:
				}
			}
		}
	}()
	return deliveries, errs, nil
}

// aiassistDelivery adapts *natsgo.Msg to aiassistinvalidator.Delivery.
type aiassistDelivery struct {
	m *natsgo.Msg
}

func (d aiassistDelivery) Data() []byte {
	if d.m == nil {
		return nil
	}
	return d.m.Data
}

func (d aiassistDelivery) MsgID() string {
	if d.m == nil {
		return ""
	}
	return d.m.Header.Get("Nats-Msg-Id")
}

func (d aiassistDelivery) Ack(_ context.Context) error {
	if d.m == nil {
		return errors.New("nats/aiassist: nil delivery")
	}
	if err := d.m.Ack(); err != nil {
		return fmt.Errorf("nats/aiassist: ack: %w", err)
	}
	return nil
}

func (d aiassistDelivery) Nak(_ context.Context, delay time.Duration) error {
	if d.m == nil {
		return errors.New("nats/aiassist: nil delivery")
	}
	if delay <= 0 {
		if err := d.m.Nak(); err != nil {
			return fmt.Errorf("nats/aiassist: nak: %w", err)
		}
		return nil
	}
	if err := d.m.NakWithDelay(delay); err != nil {
		return fmt.Errorf("nats/aiassist: nak-with-delay: %w", err)
	}
	return nil
}

// Compile-time guard: AIAssistInvalidatorSubscriber satisfies the
// worker contract.
var _ aiassistinvalidator.EventSubscriber = (*AIAssistInvalidatorSubscriber)(nil)
