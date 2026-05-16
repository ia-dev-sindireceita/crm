// Package webchat is the HTTP adapter for the embeddable Webchat
// channel — Fase 2 F2-11 of SIN-62194.
//
// The adapter exposes three public (no auth) endpoints under
// /widget/v1:
//
//   - POST /widget/v1/session  — creates an anonymous session and
//     returns a CSRF token + origin signature.
//   - POST /widget/v1/message  — receives a visitor message; validates
//     CSRF; idempotent by (session_id, client_msg_id).
//   - GET  /widget/v1/stream   — SSE stream; delivers agent replies
//     to the widget in real time; reconnection via Last-Event-ID.
//
// # Defense in depth (ADR-0021)
//
//   - CORS: Origin header validated against tenant_settings.webchat_allowed_origins.
//   - CSRF: header double-submit (X-Webchat-Session + X-Webchat-CSRF);
//     no cookies — the widget calls with credentials:'omit'.
//   - Origin signature: HMAC-SHA256(tenant_origin_secret, origin) in
//     X-Webchat-Origin-Signature; prevents signature reuse across tenants.
//   - Rate limiting: 10 sessions/min/IP, 60 msgs/min/session.
//   - Feature flag: feature.channel.webchat.enabled per tenant; flag-off →
//     404 (not 503, per ADR-0021 D7).
//   - CSP: Content-Security-Policy: default-src 'none' on all responses.
//
// # Hexagonal boundary
//
// The adapter imports only the inbox.InboundChannel port.  Identity
// resolution (channel="webchat", external_id=session_id) happens
// inside the inbox use case via the contacts.UpsertContactByChannel
// hook.  When the visitor later supplies email/phone the adapter calls
// the ContactSignalUpdater port (thin wrapper around
// contacts.IdentityRepository.Resolve).
//
// # Session storage
//
// The composition root wires a concrete SessionStore. InMemorySessionStore
// is provided for tests and early-stage deployments; the follow-up
// Postgres adapter uses migration 0096_webchat_session.
package webchat
