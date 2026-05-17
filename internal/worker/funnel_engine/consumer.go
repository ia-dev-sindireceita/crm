package funnel_engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/pericles-luz/crm/internal/funnel/engine"
)

// Delivery is one redeliverable JetStream message handed to
// [Consumer.Handle]. Ack is idempotent at the SDK layer (calling it
// twice is a no-op).
type Delivery interface {
	Data() []byte
	Ack(ctx context.Context) error
}

// Consumer is the per-delivery callback the JetStream subscription
// installs. The struct holds no mutable state across calls; tests can
// build one with a fake [engine.Engine] surface via the
// [EngineHandler] alias if needed.
type Consumer struct {
	engine *engine.Engine
	logger *slog.Logger
}

// NewConsumer wires the consumer to its engine. nil engine is rejected
// so cmd/server fails fast at boot.
func NewConsumer(e *engine.Engine, logger *slog.Logger) (*Consumer, error) {
	if e == nil {
		return nil, errors.New("funnel_engine: engine is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Consumer{engine: e, logger: logger}, nil
}

// Handle decodes the wire envelope, dispatches to the engine, and
// Acks the delivery on success.
//
// Return semantics (mirrors wallet_alerter):
//
//   - nil  → ack the delivery (success or poison)
//   - err  → leave unacked so JetStream redelivers after AckWait
func (c *Consumer) Handle(ctx context.Context, d Delivery) error {
	if d == nil {
		return errors.New("funnel_engine: nil delivery")
	}
	body := d.Data()
	if len(body) == 0 {
		c.logger.WarnContext(ctx, "funnel_engine: empty payload, dropping")
		return d.Ack(ctx)
	}
	msg, err := engine.DecodeInboundMessage(body)
	if err != nil {
		// Poison: log and Ack. Raw bytes intentionally omitted from
		// the log (may contain tenant-scoped material).
		c.logger.WarnContext(ctx, "funnel_engine: malformed payload",
			"err", err.Error(),
			"bytes", len(body),
		)
		return d.Ack(ctx)
	}
	if err := c.engine.Handle(ctx, msg); err != nil {
		// Validation error inside the engine is treated as poison
		// too — the wire decoder already validated everything we
		// know how to validate, so an engine-level ErrInvalidEvent
		// means the message lost a field between decode and
		// Engine.validate (impossible without bug, but defensive).
		if errors.Is(err, engine.ErrInvalidEvent) {
			c.logger.WarnContext(ctx, "funnel_engine: engine rejected payload",
				"err", err.Error(),
				"tenant_id", msg.TenantID,
				"message_id", msg.MessageID,
			)
			return d.Ack(ctx)
		}
		c.logger.ErrorContext(ctx, "funnel_engine: handle failed",
			"err", err.Error(),
			"tenant_id", msg.TenantID,
			"message_id", msg.MessageID,
		)
		return fmt.Errorf("funnel_engine: handle: %w", err)
	}
	return d.Ack(ctx)
}
