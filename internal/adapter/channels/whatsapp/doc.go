// Package whatsapp is the inbound HTTP adapter for the WhatsApp Cloud
// API (Meta) channel — Fase 1 PR6 of SIN-62193.
//
// The package is a boundary adapter in the hexagonal sense: it speaks
// HTTP and Meta's wire format on one side, and the inbox.InboundChannel
// port on the other. It MUST NOT import inbox.Conversation or
// inbox.Message types directly; the only inbox-side coupling permitted
// here is the InboundChannel / InboundEvent pair declared in
// internal/inbox/port_inbound.go.
//
// Design notes
//
//   - Synchronous flow. The handler verifies HMAC, validates timestamp,
//     parses the Meta envelope, resolves each message's tenant via
//     phone_number_id and the tenant_channel_associations table, then
//     calls inbox.InboundChannel.HandleInbound directly. ADR 0087 §D3
//     allows but does not require a NATS-mediated split; Fase 1 ships
//     the simpler synchronous form because (a) ReceiveInbound's work is
//     dominated by a single Postgres round-trip, (b) Meta's 5-second
//     ack budget is comfortable, and (c) no consumer worker is wired
//     yet. The two-layer dedup invariant from ADR 0087 still holds —
//     the (channel, channel_external_id) UNIQUE on inbound_message_dedup
//     prevents duplicate Message rows whether the handler runs once or
//     a thousand concurrent retries hit it.
//   - Anti-enumeration. Every drop path (parse error, unknown phone
//     number id, feature flag off, rate-limit denial, dedup hit,
//     downstream failure) returns 200 OK. The only non-200 response is
//     401 on HMAC failure — the carrier needs that signal to surface a
//     misconfigured app secret in their dashboard.
//   - Body bit-exactness. The raw body is read once before any parsing,
//     hex-decoded then constant-time-compared with hmac.Equal. Parsing
//     happens only after the HMAC is good.
//   - No PII in URLs / logs. The handler logs phone_number_id and
//     wamid (Meta-side identifiers) plus tenant_id; the message body
//     and contact name never leave the request scope without going
//     through the inbox aggregate's masking rules.
//
// SecurityEngineer review covers HMAC verification, replay-window
// math, the verify-token handshake, the rate-limit middleware, and the
// feature-flag fail-closed default.
package whatsapp
