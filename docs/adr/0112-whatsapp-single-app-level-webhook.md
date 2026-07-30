# ADR 0112 — WhatsApp Cloud API: single app-level webhook with `phone_number_id` multi-tenant fan-out

- Status: Accepted
- Date: 2026-07-30
- Deciders: CTO
- Drives: [SIN-68299](/SIN/issues/SIN-68299) (design confirmation + go-live checklist)
- Motivated by: [SIN-67470](/SIN/issues/SIN-67470) board question — "would a single webhook for all our users be better?"
- Lenses: **Secure-by-default API**, **Hexagonal / Ports & Adapters**, **Boring technology**

## Context

The Meta Cloud API (WhatsApp Business Platform) enforces exactly **one callback URL per Meta App**. Every WABA subscribed to a given App delivers events to the same endpoint — there is no per-WABA or per-tenant webhook registration. Multi-tenant routing must therefore happen inside the application, not at the HTTP layer.

The board surfaced this as an open question in [SIN-67470](/SIN/issues/SIN-67470), and this ADR records both the platform constraint and the design that exploits it correctly.

## Decision

Use a **single public endpoint** (`/webhooks/whatsapp`) that serves all tenants simultaneously. Tenant routing is performed at ingestion time by resolving the `phone_number_id` in each event payload to a tenant UUID via the `tenant_channel_associations` table.

## Implementation (already shipped)

The design is **not a new proposal** — it was implemented as part of the WA Cloud API onboarding work ([SIN-67138](/SIN/issues/SIN-67138)) and is live on the fork. The code artifacts are:

| Concern | File |
|---|---|
| HTTP handler (GET verify + POST intake) | `internal/adapter/channels/whatsapp/handler.go` |
| `phone_number_id` → tenant fan-out | `internal/adapter/channels/whatsapp/tenant_resolver.go` |
| Wire-up on the public listener | `cmd/server/whatsapp_wire.go` (lines 161–168) |
| HMAC + verify-token config | `internal/adapter/channels/whatsapp/config.go` |
| GET challenge handler | `internal/adapter/channels/whatsapp/challenge.go` |

### Endpoint

```
GET  /webhooks/whatsapp   — Meta hub-mode challenge
POST /webhooks/whatsapp   — event intake for all WABAs
```

Both are registered on the **public** listener (unauthenticated inbound path), which is the only option: Meta does not send authentication credentials that could satisfy our session/JWT gate.

### Security model

- **Authenticity** — every POST is verified with `HMAC-SHA256(META_APP_SECRET, raw_body)` against the `X-Hub-Signature-256` header before the body is parsed. Requests without a valid signature are rejected with `403`. This is the ONLY signature key for the entire app; there is no per-tenant secret because Meta does not provide one.
- **Challenge** — GET verify uses a single `META_VERIFY_TOKEN` known to both this application and the Meta App dashboard. Tokens are environment variables, never hard-coded.
- **Tenant isolation** — once authenticity is confirmed, the `phone_number_id` from `entry[].changes[].value.metadata.phone_number_id` is resolved to a tenant UUID by `pgTenantResolver`. Events that resolve to an unknown `phone_number_id` are discarded (404 or no-op, logged for observability). Cross-tenant leakage is impossible because each event carries exactly one `phone_number_id` and the resolver enforces a single mapping.
- **Idempotency** — Meta retries on non-200 responses. The intake handler returns `200 OK` after HMAC verify, before slow processing, to suppress retries; processing errors are handled asynchronously.

### Why not per-tenant webhook URLs?

- The Meta platform does not support it. A per-tenant URL would require a separate Meta App per tenant, which multiplies App-level secret management and subscription overhead by the tenant count.
- Single-app model is the standard documented approach for SaaS providers on Meta Cloud API.

## Alternatives considered

| Option | Verdict |
|---|---|
| One Meta App per tenant (per-tenant URL) | Rejected — platform does not scale this way; O(N) App registrations, secret rotation complexity, and Meta imposes App-level review/rate limits. |
| Route by subdomain (`<tenant>.webhooks.example.com`) | Rejected — Meta Cloud API binds the URL at App creation; wildcard subdomains would still share the same App secret and add DNS complexity with no security benefit. |
| Per-tenant signing secret (forward-proxy pattern) | Not applicable — Meta signs with a single App secret; there is no per-subscriber secret to forward. |

## Go-live checklist (operational, not code)

These steps are performed by the operator at deploy time; no code change is required.

1. Deploy with `META_APP_SECRET` and `META_VERIFY_TOKEN` populated. Confirm log line `crm: whatsapp intake mounted on public listener`.
2. In the Meta App dashboard → WhatsApp → Configuration → set Callback URL to `https://<host>/webhooks/whatsapp` and Verify Token to `META_VERIFY_TOKEN`. Meta will send a GET challenge; confirm `200` in the dashboard.
3. Subscribe the App to the target WABA(s) and enable the `messages` webhook field.
4. For each WhatsApp Business phone number, insert a row in `tenant_channel_associations` (channel = `whatsapp`, association_key = `phone_number_id` value, tenant_id = tenant UUID). This can be done via the `/settings/channels` onboarding UI ([SIN-67144](/SIN/issues/SIN-67144)) or via direct seed migration.
5. End-to-end smoke: send a real message to a subscribed number → verify `200` on the intake → confirm conversation appears in the correct tenant's inbox.

## Consequences

- **Positive.** Single endpoint is operationally simple: one URL to register with Meta, one HMAC secret to rotate, one log stream to monitor. Tenant fan-out is a cheap DB lookup (indexed on `phone_number_id`).
- **Positive.** The hexagonal boundary is clean: the HTTP adapter resolves tenant context and hands a domain event to the use-case layer; the use-case layer has no knowledge of `phone_number_id` or Meta's payload format.
- **Negative / watch.** A single `META_APP_SECRET` is a shared credential. Rotation requires a coordinated update (new secret in Meta dashboard + env var + restart) with a brief dual-accept window. Document in runbook; rotation is low-frequency (Meta does not force rotation on a schedule).
- **Negative / watch.** If a tenant's `phone_number_id` is not in `tenant_channel_associations`, inbound events are silently dropped. The intake handler must emit an observable warning log (`unknown phone_number_id`) to surface misconfiguration.
