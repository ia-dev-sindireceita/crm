// Package wa_session is the inbound/outbound adapter for the
// non-official WhatsApp Web ("session") channel — Fase 2 of the
// WhatsApp-sem-API track (SIN-66257, plan rev 4 of SIN-66252,
// ratified by ADR 0107).
//
// It is a boundary adapter in the hexagonal sense. On the domain side
// it consumes inbox.InboundChannel (to deliver received messages) and
// implements inbox.OutboundChannel (to send messages). On the carrier
// side it talks to a single small port, SessionSender, which the
// whatsmeow session manager (Fase 1) implements.
//
// Deliberate decoupling from whatsmeow
//
//	This package MUST NOT import go.mau.fi/whatsmeow. The session
//	component (Fase 1) owns every whatsmeow type, the QR pairing, the
//	Postgres session store and the WebSocket lifecycle. It translates
//	whatsmeow events into the carrier-neutral SessionMessage value and
//	calls Adapter.Receive; for outbound it satisfies SessionSender by
//	mapping an E.164 string into a whatsmeow JID and calling the lib.
//	Keeping whatsmeow out of this package is what makes the adapter
//	unit-testable with table-driven fakes and no live session — the AC
//	of SIN-66257 ("testes table-driven sem DB") — and what lets Fase 2
//	land while Fase 1 (SIN-66256) is still in progress.
//
// Coexistence with the official Meta Cloud channel (ADR 0107 D4)
//
//	The session channel registers under the SAME channel string as the
//	official adapter (Channel == contacts.ChannelWhatsApp == "whatsapp")
//	so a contact's WhatsApp thread is unified regardless of which
//	provider carried the message. The two adapters are distinguished by
//	Provider ("meta" vs "session"); Fase 3 selects the adapter per
//	tenant. Because the channel string is shared, the domain dedup
//	ledger (channel, channel_external_id) and the E.164 contact
//	identity rules apply unchanged.
//
// Security bar (SIN-66257)
//
//   - Input validation at the border. Inbound: the sender phone is
//     normalised to strict E.164 (whatsmeow exposes bare digits / a
//     JID; the domain requires a leading '+'), group and self-echo
//     events are dropped, and an empty message id is dropped. Outbound:
//     recipient E.164 and non-empty body are validated before the lib
//     is touched.
//   - Tenant scope. Inbound TenantID is supplied by the tenant-bound
//     session (Fase 1); outbound TenantID comes from the use case and
//     is passed to SessionSender so the correct tenant's whatsmeow
//     client sends. The adapter never crosses tenants and holds no
//     ambient tenant state.
//   - Deny-by-default. A per-tenant FeatureFlag gates the channel; an
//     unconfigured tenant is off on both directions.
//   - Ban-risk mitigation. Outbound is rate-limited per tenant — the
//     plan's ToS/ban risk (accepted by the board 2026-06-29) is made
//     worse by burst sending, so the cap throttles a single tenant's
//     session.
//
// No PII in logs: the adapter logs tenant_id, the whatsmeow message id
// and the channel/provider only; message bodies and phone numbers are
// never written to the log sink.
package wa_session
