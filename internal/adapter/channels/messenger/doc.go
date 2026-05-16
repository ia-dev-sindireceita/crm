// Package messenger is the inbound HTTP adapter for the Facebook
// Messenger channel via Meta Cloud API — Fase 2 F2-10 of SIN-62194.
//
// The package is a boundary adapter in the hexagonal sense: it speaks
// HTTP and Meta's Messenger envelope on one side, and the
// inbox.InboundChannel port on the other. It MUST NOT import inbox
// entity types (Conversation, Message) directly — the only inbox-side
// coupling permitted is InboundChannel / InboundEvent from
// internal/inbox/port_inbound.go.
//
// # Wire format
//
// The Messenger envelope shape differs from the WhatsApp one: the
// per-entry collection key is `messaging[]` (not `changes[].value`),
// each item carries a `sender.id` (page-scoped id / PSID) instead of a
// phone number, and timestamps are encoded in milliseconds rather than
// seconds. The dedup identifier is `message.mid`. The page-scoped id
// is opaque from Meta's side; it maps 1:1 to a Contact via the
// Identity hook (channel="messenger", external_id=psid).
//
// # Tenant resolution
//
// The recipient (`entry[].id`, the page id) is the tenant association
// key: a Facebook Page is owned by exactly one tenant in
// tenant_channel_associations. The handler looks up the tenant from
// the page id before invoking the InboundChannel port.
//
// Defense in depth
//
//   - HMAC-SHA256 over the raw body, key = META_APP_SECRET, header =
//     X-Hub-Signature-256 (shared with WhatsApp). The metashared
//     package owns the constant-time comparison.
//   - Anti-enumeration: every drop path returns 200 OK. The only
//     non-200 is 401 on HMAC failure (the carrier needs that signal
//     to surface a misconfigured app secret in the Meta dashboard).
//   - Replay window: entries older than PastWindow or further in the
//     future than FutureSkew are dropped. Trusting Meta's clock here
//     is safe because Meta signs the envelope, including the
//     timestamps, with the app secret.
//   - Feature flag (FEATURE_MESSENGER_ENABLED + tenant allowlist)
//     fail-closed so flipping the flag off stops Message
//     materialisation immediately.
//
// Sender + inbound media are intentionally NOT in this PR. The webhook
// receiver lands first to respect the F2-10 <400 LoC budget; the
// follow-up child issue ships the OutboundChannel implementation and
// the MediaScanner integration that F2-05 already provides.
package messenger
