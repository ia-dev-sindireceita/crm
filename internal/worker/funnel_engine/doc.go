// Package funnel_engine wires the funnel rule engine
// (internal/funnel/engine) to a JetStream subscription on
// [engine.Subject]. Each delivery decodes into an
// [engine.InboundMessage], hands it to [engine.Engine.Handle], and
// Acks the JetStream message on success.
//
// Hexagonal layout:
//
//   - Inputs (Delivery, Subscriber) are narrow interfaces. The NATS
//     adapter (internal/adapter/messaging/nats) implements them; tests
//     use in-process fakes or the embedded JetStream server.
//   - Outputs go through the engine package's ports — funnel rules
//     resolver, applications repo, stage mover. The worker only
//     translates JSON bytes into the domain shape.
//
// Failure-mode contract (mirrors internal/worker/wallet_alerter):
//
//   - Malformed JSON or a missing required field is poison: the worker
//     logs a Warn and Acks the delivery so JetStream does not redeliver
//     a payload that will never decode.
//   - An engine error is transient: the worker returns the error to the
//     SDK adapter so JetStream redelivers after AckWait. The (rule_id,
//     message_id) UNIQUE constraint inside the engine collapses
//     redeliveries into a single applied row.
//
// SIN-62960 (Fase 4 funnel rule engine — NATS consumer, child of
// [SIN-62197](/SIN/issues/SIN-62197)).
package funnel_engine
