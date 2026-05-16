// Package instagram is the inbound HTTP adapter for the Instagram Direct
// channel via Meta's Messaging Platform — Fase 2 F2-09 of SIN-62193
// (issue SIN-62796).
//
// The package is a boundary adapter in the hexagonal sense: it speaks
// HTTP and Meta's Messaging-Platform wire format on one side, and the
// inbox.InboundChannel port on the other. It MUST NOT import the
// conversation/message entity types directly, nor the whatsapp,
// messenger, or webchat sibling adapter packages — every cross-channel
// concern (signature verification, IP allowlist, dedup) lives in
// internal/adapter/channels/metashared (F2-02).
//
// Design notes
//
//   - Synchronous flow. The handler verifies HMAC, parses the IG
//     envelope (`entry[].messaging[]` Messenger-style shape, NOT the
//     WhatsApp Cloud `changes[]` shape), resolves each message's tenant
//     via the recipient IG Business Account id and the
//     tenant_channel_associations table, then calls
//     inbox.InboundChannel.HandleInbound. Identity resolution happens
//     inside the inbox use case (which delegates to F2-06
//     contacts.IdentityRepository) — the adapter passes
//     Channel="instagram" and SenderExternalID=IGSID.
//   - Anti-enumeration. Every drop path (parse error, unknown ig
//     business id, feature flag off, rate-limit denial, dedup hit,
//     downstream failure) returns 200 OK. The only non-200 response is
//     401 on HMAC failure — the carrier needs that signal to surface a
//     misconfigured app secret in the Meta dashboard.
//   - Media handling. Each inbound message.attachments[] entry fires a
//     `media.scan.requested` envelope via the MediaScanPublisher port
//     (F2-05). The InboundEvent.Body holds a deterministic placeholder
//     (e.g. "[image]") so the conversation thread row stays non-empty
//     while the scan resolves the message to `clean` (post-clean
//     re-materialisation ships in a follow-up PR).
//   - Reversibility. feature.channel.instagram.enabled gates the inbound
//     path globally + per-tenant. Flag off → handler still verifies HMAC
//     and dedups but skips Message materialisation.
package instagram
