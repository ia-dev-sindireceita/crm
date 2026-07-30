// Package dispatch assembles the outbound message dispatcher that sits
// behind the inbox send-outbound use case (SIN-68306).
//
// The use case (internal/inbox/usecase.SendOutbound) speaks to a single
// inbox.OutboundChannel port. Production needs three concerns layered on
// top of a raw carrier Sender (e.g. internal/adapter/channel/whatsapp):
//
//   - routing by conversation channel, so one SendOutbound can serve
//     WhatsApp, Messenger, … without the use case importing any carrier
//     SDK (Router);
//   - outbound rate limiting, so an operator burst or a requeue storm
//     cannot exceed the tenant's per-minute carrier budget (RateLimited);
//   - app-side idempotency, so a retry or an operator double-submit that
//     carries the same OutboundMessage.IdempotencyKey reaches the carrier
//     at most once — Meta's Graph API has no server-side idempotency key
//     (Idempotent + Ledger).
//
// Every type in this package implements inbox.OutboundChannel, so they
// compose: Router{"whatsapp": Idempotent(RateLimited(waSender))}. The
// routing/decoration logic lives here in the adapter layer, never in the
// use case — keeping the hexagonal dependency direction intact.
//
// Reversibility / deny-by-default: an empty Router (no routes wired,
// e.g. META_GRAPH_TOKEN unset or the feature flag off) answers every
// SendMessage with inbox.ErrChannelDisabled and issues zero carrier HTTP,
// so wiring the dispatcher flag-off is a safe no-op.
package dispatch
