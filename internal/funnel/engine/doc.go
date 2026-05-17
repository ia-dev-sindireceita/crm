// Package engine is the funnel rule engine that turns inbound messages
// into automatic stage transitions, per SIN-62960 (Fase 4 child of
// [SIN-62197]).
//
// Layout (hexagonal):
//
//   - [InboundMessage] is the domain event the engine evaluates against
//     rules. Wire-side (NATS) it lands as [InboundMessageEvent]; the
//     consumer in internal/worker/funnel_engine decodes the wire shape
//     into an [InboundMessage] before handing it to [Engine.Handle].
//   - Ports: [ApplicationsRepo] for the idempotency ledger,
//     [StageMover] for the action dispatch, and the existing
//     [github.com/pericles-luz/crm/internal/funnel/rules.Resolver] for
//     the cascade lookup. The engine never imports pgx or NATS — those
//     plug in via the ports.
//   - Adapters: postgres adapter for [ApplicationsRepo] lives in
//     internal/adapter/db/postgres/funnelapplications; the NATS
//     publisher that lets inbox.ReceiveInbound emit
//     [InboundMessageEvent] envelopes lives in
//     internal/adapter/messaging/nats; the consumer + entrypoint live
//     under internal/worker/funnel_engine and cmd/funnel-engine-worker.
//
// Idempotency: every successful action records a row in
// funnel_rule_applications keyed by UNIQUE (rule_id, message_id). The
// engine treats both an at-the-start [ApplicationsRepo.IsApplied] hit
// and an end-of-pipeline UNIQUE conflict as a no-op skip. Combined with
// JetStream's queue-group serialization, that gives at-most-once
// dispatch per (rule, message) pair across redeliveries.
//
// Goroutine-safety: [Engine.Handle] holds no mutable state across
// calls; the embedded resolver / repos / mover are expected to be
// concurrency-safe (the production pgx adapters and funnel.Service
// already are). The worker fan-in is naturally serialized by JetStream
// AckWait + queue groups, but the engine itself does not require it —
// nothing in the type is gated by a sync.Mutex.
//
// Observability: every Handle call emits the four metrics declared in
// metrics.go (evaluated, matched, applied, latency). The worker
// process exposes them on its own /metrics listener.
//
// SIN-62960 (Fase 4 funnel rule engine — NATS consumer, child of
// [SIN-62197](/SIN/issues/SIN-62197)).
package engine
