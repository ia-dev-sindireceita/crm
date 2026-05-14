# ADR 0087 — Inbound webhook idempotency: canonical key + retry contract

- Status: accepted
- Date: 2026-05-14
- Deciders: CTO
- Tickets: [SIN-62723](/SIN/issues/SIN-62723) (this ADR), [SIN-62193](/SIN/issues/SIN-62193) (Fase 1 parent),
  [SIN-62224](/SIN/issues/SIN-62224) (ADR 0075 — webhook security predecessor),
  [SIN-62220](/SIN/issues/SIN-62220#document-security-review) (F20/F21/F23/F-1..F-14 review record),
  [SIN-62234](/SIN/issues/SIN-62234) / [SIN-62279](/SIN/issues/SIN-62279) (reconciler + JetStream dedup landed).

## Context

ADR 0075 (`webhook-security`) closed F20/F21/F23 plus the rev-2/rev-3 finding
batch on the **transport/security** axis: opaque per-tenant URL token, HMAC
verification with declared `SecretScope`, replay defense via 5-minute
timestamp window, body bit-exactness, NATS `Nats-Msg-Id` dedup, and the
`raw_event.published_at` outbox-lite reconciler.

What ADR 0075 deliberately left *implicit* — because it was already long
enough — is the **inbound domain idempotency contract** that the inbox uses
to decide whether a given webhook event has produced a message yet. Fase 1
adds the first concrete consumer (WhatsApp via Meta Cloud) and a second
storage axis (`inbound_message_dedup`, populated by the inbox consumer
worker, not the webhook handler). Without a canonical statement of the
domain-side key, the inbox consumer and the webhook handler risk drifting
into incompatible idempotency semantics:

- The **webhook handler** dedupes `(tenant_id, channel, sha256(raw_payload))`
  in `webhook_idempotency` (ADR 0075 §D2). That is "we already received and
  persisted this exact body."
- The **inbox consumer** needs a different invariant: "we already produced a
  `Message` for this provider-side event id (`wamid` for WhatsApp), even if
  the carrier re-delivered the *same* event with byte-different bodies"
  (e.g., Meta re-signs and re-times the envelope while keeping the inner
  message id stable). The webhook-handler key cannot answer that question on
  its own.

This ADR closes that gap. It also fixes the retry/ack contract from the
consumer side so the carrier (Meta Cloud) is acknowledged the instant the
webhook handler has persisted the dedup row — never delayed by downstream
inbox processing. The retry contract is what protects the **carrier**'s
delivery SLO; the canonical key is what protects the **inbox**'s "no
duplicate Message" invariant.

The Fase 1 plan in [SIN-62193](/SIN/issues/SIN-62193) acceptance criterion
§5 explicitly requires that webhook re-delivery of the same `wamid` does not
duplicate `Message` rows or wallet debits. That requirement is the test of
record for this ADR.

## Decision

### D1 — Two-layer idempotency, two distinct keys

Inbound webhook processing keeps **two** idempotency layers, and they use
**different** canonical keys on purpose:

1. **Transport layer (webhook handler).** Owned by ADR 0075 §D2.
   `webhook_idempotency(tenant_id, channel, idempotency_key)` where
   `idempotency_key = sha256(tenant_id || ':' || channel || ':' || raw_payload)`.
   Insert-on-conflict-do-nothing returning. If the row is new, the handler
   continues to persist `raw_event` and publish to NATS. If the row
   pre-exists, the handler returns 200 immediately without any side effect.
   This layer's job is: "did we already accept this byte-identical envelope?"
2. **Domain layer (inbox consumer).** **Owned by this ADR.**
   `inbound_message_dedup(tenant_id, channel, channel_external_id) UNIQUE`,
   where `channel_external_id` is the **provider-assigned event identifier**
   declared by the channel adapter (see D2). For WhatsApp via Meta Cloud,
   that is `wamid`. The inbox consumer worker inserts this row inside the
   same transaction that produces the `Message` / `Conversation` /
   `Assignment` rows in `internal/inbox`. Insert-on-conflict-do-nothing.
   If the row pre-exists, the consumer ACKs the NATS message without
   creating a second `Message`. This layer's job is: "did we already
   materialise a domain `Message` for this provider event?"

The two keys answer different questions. Collapsing them into one
`webhook_idempotency` row would force the consumer worker to either re-hash
the payload to recover its idempotency key (which couples the inbox to the
exact byte-form of the inbound payload, breaking carrier re-signing) or to
trust the transport layer's dedup as a proxy for domain dedup (which is
wrong: byte-different envelopes for the same `wamid` would each create a
`Message`).

### D2 — Canonical key `(channel, channel_external_id)` with UNIQUE

```sql
CREATE TABLE inbound_message_dedup (
    tenant_id           uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    channel             text        NOT NULL,                       -- enum-fechado, ver ADR 0075 §D2
    channel_external_id text        NOT NULL,                       -- provider-assigned id (wamid, etc.)
    message_id          uuid        NOT NULL,                       -- internal Message.id produced
    inserted_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (channel, channel_external_id)
);
CREATE INDEX inbound_message_dedup_tenant_idx
  ON inbound_message_dedup (tenant_id, channel, inserted_at DESC);
```

The PRIMARY KEY is `(channel, channel_external_id)` — **not** scoped by
`tenant_id`. A provider-assigned event id is, by construction, globally
unique within a channel (Meta documents `wamid` as unique across all Meta
Cloud accounts). Including `tenant_id` in the key would let a misrouted
event from tenant A's webhook materialise a `Message` for tenant B if the
URL/token leak attack from F-12 ever bypassed the body-tenant cross-check
in ADR 0075 §D4. Defense in depth: the unique key alone rejects the second
materialisation regardless of which tenant attempted it. The `tenant_id`
column stays as a non-key attribute for joins and forensics; the supporting
index covers per-tenant lookup.

#### Adapter contract addition

ADR 0075 §D4 already defines `ChannelAdapter.ParseEvent(body) (Event,
error)`. This ADR extends the `Event` struct (in `internal/webhook` or
wherever the inbox port is defined — implementation choice, the ADR fixes
the shape only) with:

```go
type Event struct {
    // ... existing fields from ADR 0075
    ChannelExternalID string // adapter MUST populate; non-empty invariant
    // ... message-shape fields owned by the inbox port
}
```

Empty `ChannelExternalID` is a programming error: the inbox consumer
**rejects** events with `ChannelExternalID == ""` and logs
`inbox.missing_external_id` with adapter name. Lint custom
(`paperclip-lint missing-channel-external-id`) flags any adapter
implementation whose `ParseEvent` returns an `Event` without setting that
field.

For WhatsApp via Meta Cloud, the adapter populates
`ChannelExternalID = entry[].changes[].value.messages[].id` (the `wamid.*`
string). For future adapters (Instagram, Facebook, webchat, PSP webhooks),
the adapter declares which provider field is the canonical event id in its
godoc.

### D3 — Retry contract: 2xx after transport-dedup row, async after that

The carrier ack contract from ADR 0075 §D6 is restated here in
domain-aware form so the boundary is unambiguous when an engineer is
reading the inbox code path:

1. Webhook handler verifies HMAC + timestamp + tenant body cross-check
   (ADR 0075 §D3/§D4).
2. Webhook handler inserts `webhook_idempotency` and `raw_event` (ADR 0075
   §D2/§D7). **The 200 OK is returned at this point**, not later.
3. Webhook handler publishes `raw_event.id` to NATS as `Nats-Msg-Id`
   (ADR 0075 §D7), then updates `raw_event.published_at`. If NATS publish
   fails, the reconciler retries; the carrier is **not** asked to redeliver.
4. The inbox consumer worker reads the NATS message, inside one DB
   transaction:
   - Parses the body through the channel adapter to get
     `(ChannelExternalID, ParsedEvent)`.
   - Inserts `inbound_message_dedup(channel, channel_external_id, ...)`
     with `ON CONFLICT DO NOTHING`. If the row was already present
     (`RETURNING` empty), the worker commits and ACKs the NATS message
     with `outcome=inbox.duplicate_external_id` — no `Message` created.
   - On fresh insert: persists `Message`, `Conversation`,
     `Assignment` rows, debits the wallet (see ADR 0088), commits the
     transaction, ACKs the NATS message.
5. If the consumer worker crashes between NATS ACK and DB commit, NATS
   redelivers and `inbound_message_dedup` swallows the dup. If the worker
   crashes between DB commit and NATS ACK, the dup again hits
   `inbound_message_dedup` after redelivery and is dropped.

The carrier sees 200 within p95 ≤ 100 ms (ADR 0075 target). Carrier retries
are absorbed by `webhook_idempotency` (transport layer). NATS retries are
absorbed by `inbound_message_dedup` (domain layer). The two layers compose:
no single failure mode produces a duplicate `Message`.

### D4 — Relationship to ADR 0075 (delta)

ADR 0075 is the antecedent for inbound webhook security and the
transport-layer idempotency table (`webhook_idempotency`). This ADR is the
**successor** specifically for the inbox-channel domain semantics: it adds
`inbound_message_dedup`, the `ChannelExternalID` adapter invariant, and
the retry contract as a domain-aware restatement of ADR 0075 §D6.

ADR 0075 stands as written; nothing in this ADR retracts a decision there.
Concretely, this ADR adds:

- One new table (`inbound_message_dedup`), separate from the four ADR 0075
  tables.
- One new adapter invariant (`Event.ChannelExternalID` non-empty),
  enforceable by lint.
- Domain restatement of the ack contract that ADR 0075 expressed in
  transport terms.

Future webhook ADRs that touch inbound channels should cite **both** as
antecedents (transport contract from 0075, domain contract from 0087).
Outbound message dispatch and provider status reconciliation
(sent/delivered/read/failed) are out of scope and will get their own ADR
when Fase 1 lands the outbound path.

### D5 — Feature flag

The flag `feature.whatsapp.enabled` (per tenant, ADR 0088 D5 references it
too) gates the **entire** inbound WhatsApp path: webhook route accepts
events, consumer worker materialises `Message` rows, wallet debits fire.
Flipping off the flag stops materialisation but does **not** stop transport
dedup or `raw_event` persistence — replay safety must survive flag flips.
That is why `inbound_message_dedup` is populated by the consumer worker,
not the webhook handler: turning the flag off mid-burst leaves no domain
side effects to roll back.

## Consequences

Positive:

- Two-layer idempotency makes the inbox safe against both carrier replay
  (transport layer absorbs) and NATS replay / worker crash (domain layer
  absorbs). Neither layer needs to know about the other's failure modes.
- The canonical key `(channel, channel_external_id)` is provider-aligned.
  When a future support ticket asks "why didn't this `wamid` produce a
  message?", the engineer can grep `inbound_message_dedup` directly with
  the carrier's identifier instead of reverse-engineering a `sha256` hash.
- Removing `tenant_id` from the PRIMARY KEY closes the residual F-12 leg
  ("misrouted event still materialises if it slips body cross-check") with
  a single uniqueness constraint, defense in depth on top of ADR 0075 §D4.
- The adapter `ChannelExternalID` field is a small contract addition that
  forces every future channel adapter (Instagram, Facebook, PSP, webchat)
  to declare its event identifier explicitly. The invariant is lint-enforceable.
- The retry contract decouples carrier ack latency from inbox processing
  latency. Even if inbox queue backs up (e.g., LLM debit slow path), the
  carrier still sees ≤ 100 ms ack.

Negative / costs:

- One extra table on the write path (`inbound_message_dedup`) — but it is
  inside the inbox consumer's transaction, not the webhook handler's, so
  ack latency is unaffected.
- Future engineers will need to remember that there are **two** dedup
  tables and they are not redundant. This ADR is the artifact that
  documents the why.

## Alternatives considered

### Option B — Single dedup table at the webhook handler

Have the webhook handler compute the `channel_external_id` (by partially
parsing the body) and store it in `webhook_idempotency` so the consumer
worker can dedup on the same row.

Rejected because:

- The webhook handler must remain hexagonal-pure with respect to the inbox.
  It cannot call `ChannelAdapter.ParseEvent` without coupling transport
  to domain — that adapter call is supposed to happen inside the consumer
  worker where the inbox port owns the lifecycle. Parsing the body twice
  (once in the handler for `channel_external_id`, once in the consumer
  worker for the full event) is wasteful and creates two places where a
  parse change must be made.
- Lens **hexagonal / ports & adapters.** The webhook handler is an HTTP
  adapter for the *transport* port. The inbox consumer is an adapter for
  the *inbox* port. Having one adapter populate the dedup key of the other
  inverts the dependency.
- The transport layer dedup and the domain layer dedup answer different
  questions (byte-identical envelope vs. semantically-identical event).
  Forcing one row to encode both questions creates ambiguity for engineers
  later trying to add a new channel where the carrier re-signs envelopes.

### Option C — Rely solely on `webhook_idempotency`

Skip `inbound_message_dedup` entirely and trust that ADR 0075's transport
dedup is enough to prevent duplicate `Message` rows.

Rejected because:

- It assumes the carrier never re-signs an envelope for the same event.
  Meta documents that webhooks can be redelivered with refreshed timestamps
  and (in some failure modes) refreshed entry-level fields while keeping
  the inner `wamid` stable. Two byte-different bodies with the same
  `wamid` would each pass `webhook_idempotency` and each produce a
  `Message`. That violates SIN-62193 §5 acceptance.
- Lens **defense in depth.** A single dedup constraint on the wrong axis
  (envelope) does not protect the invariant that matters (one Message per
  provider event id). The domain layer must own its own constraint.

### Option D — Dedup on `Message` table directly

Add `UNIQUE(tenant_id, channel, channel_external_id)` on `internal/inbox`'s
`Message` table and skip `inbound_message_dedup`.

Rejected because:

- The inbox consumer needs to swallow duplicates *before* deciding whether
  to debit the wallet. If the dedup constraint lives on `Message`, the
  consumer either has to attempt the insert and catch the
  unique-violation error (then roll back the wallet debit it already
  staged in the same TX), or has to pre-check `Message` with a separate
  query before the wallet debit (which races against another worker
  doing the same).
- A dedicated `inbound_message_dedup` table is **inserted first** in the
  TX. If it conflicts, the rest of the TX (Message + wallet debit) is
  never even staged. That is simpler and cheaper than the rollback path
  and matches the wallet contract in ADR 0088.
- Lens **boring technology budget.** A dedup table is the textbook
  pattern. Reusing the `Message` table's unique constraint as a
  composite-purpose dedup mechanism is clever-in-the-bad-way.

## Lenses cited

- **Hexagonal / ports & adapters.** Transport adapter owns
  `webhook_idempotency`; inbox adapter owns `inbound_message_dedup`. The
  domain core (`internal/inbox`) declares the port and does not import
  either adapter or `database/sql`.
- **Idempotency.** Canonical key with UNIQUE constraint, retry contract
  declared per layer.
- **Defense in depth.** Two independent dedup mechanisms compose; either
  alone is insufficient against the full set of failure modes.
- **Reversibility & blast radius.** `feature.whatsapp.enabled` per tenant
  lets us flip off inbound materialisation in seconds without rolling
  back transport dedup, which keeps replay safety during the flap.
- **Secure-by-default.** `(channel, channel_external_id)` UNIQUE without
  `tenant_id` closes the residual cross-tenant materialisation leg from
  F-12 even if the body cross-check ever has a bug.
- **Boring technology budget.** Plain Postgres UNIQUE + `ON CONFLICT DO
  NOTHING`. No advisory locks, no Redis, no distributed coordination.

## Rollback

If `inbound_message_dedup` semantics turn out to be wrong (e.g., a future
channel discovers that its provider event id is not globally unique within
the channel as we assumed), the rollback path is:

1. Add a migration that drops the existing PRIMARY KEY and re-creates it
   as `(tenant_id, channel, channel_external_id)`. The existing rows
   remain valid under the broader key. This is forward-only — pre-rollback
   rows that contained a provider-id collision across tenants would have
   already produced a duplicate `Message`, but the new constraint prevents
   future ones.
2. Update the adapter godoc for the affected channel to declare the new
   scoping.

A full rollback to "transport-only dedup" (Option C) is not supported —
that path was rejected for durable reasons.

## Out of scope

- Outbound message dispatch and provider-status reconciliation (sent /
  delivered / read / failed) — separate ADR when the Fase 1 outbound path
  lands.
- The contents of `Message`, `Conversation`, and `Assignment` aggregates —
  owned by `internal/inbox`; this ADR fixes only the dedup table and the
  adapter invariant.
- The GC policy for `inbound_message_dedup` — ADR 0089 covers retention.
