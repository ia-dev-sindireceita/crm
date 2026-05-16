# ADR 0043 — AI assist token débito: reserve → commit/rollback atomic protocol

- Status: Accepted
- Date: 2026-05-16
- Deciders: CTO
- Drives: [SIN-62901](/SIN/issues/SIN-62901) (this ADR), [SIN-62196](/SIN/issues/SIN-62196) (Fase 3 parent)
- Builds on: [ADR 0088](./0088-wallet-basic.md) (wallet aggregate, ledger, reconciler), [ADR 0097](./0097-subscription-and-invoice-model.md) (monthly allocation)
- Cross-links: [ADR 0040](./0040-llm-port-retry-idempotency.md) (the LLM call straddled by reserve/commit)

## Context

Fase 3 ([SIN-62196](/SIN/issues/SIN-62196)) adds AI assist as a
billable feature. Each LLM call ([ADR 0040](./0040-llm-port-retry-idempotency.md))
consumes tokens that must be debited from the tenant's wallet
([ADR 0088](./0088-wallet-basic.md)). The previous billable surface
— per-message wallet debit — uses the same primitives but has a
simpler shape: one message → one debit of one known cost.

AI assist breaks two of those assumptions:

- **Cost is unknown until after the call.** The token count returned
  by OpenRouter is the authoritative price; we cannot debit
  upfront because we do not know how much we owe.
- **The call can fail after the wallet thinks it has spent.** If we
  debit before calling, a provider outage means the tenant pays for
  nothing. If we debit after calling, a crash between provider-OK
  and DB-OK means the provider was used but the tenant is not
  charged.

[ADR 0088](./0088-wallet-basic.md) already specifies the reserve→commit
protocol in §D2–§D4 for the per-message flow. The aggregate's
`Reserve`, `Commit`, and `Rollback` methods enforce the
invariants. The wallet repository in
`internal/wallet/repo.go` applies them with conditional `UPDATE`
on `version` and writes the ledger row inside the same TX.

This ADR is the durable record of how the AI assist use-case
*uses* those primitives, plus the additions required by the
unknown-cost path:

1. The reserved amount is an **estimate** based on
   `policy.max_tokens_per_call` (the worst-case the policy allows).
2. The commit amount is the **actual** token count reported by the
   provider, which is ≤ the estimate.
3. The idempotency key is anchored to the *request*, not the
   *message*, so retries within a single AI call do not double-bill.
4. A zero balance is a first-class error path that surfaces to the
   caller and (in Fase 3) emits a NATS event for downstream
   reaction.

The shape is borrowed from classical reserve/capture flows in
payments (the per-message variant already uses it); this ADR
parameterises it for AI assist.

## Decision

**The AI assist use-case wraps every LLM call with
`wallet.Reserve(estimate)` → LLM call → `wallet.Commit(actual)` or
`wallet.Rollback()`. The idempotency key is
`(tenant_id, conversation_id, request_id)` — the same key
threaded through [ADR 0040](./0040-llm-port-retry-idempotency.md).
A `wallet.balance.depleted` NATS event fires on insufficient-balance
errors so downstream consumers can react.**

### D1 — Reserve

Before invoking the LLM port, the use-case calls:

```go
resv, err := wallet.Reserve(ctx, wallet.ReserveInput{
    TenantID:       req.TenantID,
    Tokens:         pol.MaxTokensPerCall,           // upper bound from policy
    IdempotencyKey: idemKey(req),                   // sha256 of the tuple, 32 hex
    Reason:         "ai_assist.reserve",
    Source:         "ai_assist",
})
```

`pol.MaxTokensPerCall` is the policy-resolved upper bound (see
[ADR 0042](./0042-policy-cascade.md)). It is the most the provider
can charge us for this call; reserving exactly this amount means
the post-call commit never undershoots.

The wallet repository performs the conditional `UPDATE` from [ADR
0088](./0088-wallet-basic.md) §D2 — `balance -= Tokens`, `reserved
+= Tokens`, `version += 1` — and writes one ledger row with
`direction='reserve'`, `state='pending'`, the idempotency key, and
`source='ai_assist'`.

`Reserve` is the same primitive the per-message flow uses; AI
assist supplies a different `Reason` and `Source` so the ledger
clearly attributes the spend. The `source` value is one of the
ledger-row enum already in the schema (per [ADR
0088](./0088-wallet-basic.md) §D4); `ai_assist` is a new value
added in the Fase 3 migration.

### D2 — Idempotency key

The key is `sha256(tenant_id || ':' || conversation_id || ':' || request_id)`,
truncated to 32 hex characters. The tuple is identical to the one
in [ADR 0040](./0040-llm-port-retry-idempotency.md) §D4 so the LLM
port and the wallet share a single end-to-end correlation id.

The ledger `UNIQUE(idempotency_key)` constraint short-circuits
duplicate `Reserve` calls. If the use-case re-enters with the same
`request_id` (operator double-click, retry middleware, page
reload), the second `Reserve` returns the existing pending
reservation rather than creating a second one. The wallet balance
and `version` are untouched on the duplicate.

This means **idempotency is enforced at the ledger UNIQUE**, not
in the application. A buggy retry that generates a fresh
`request_id` on each attempt would defeat dedup; the LLM port's
contract that `request_id` is stable across retries (D4 of [ADR
0040](./0040-llm-port-retry-idempotency.md)) is the matching half
of this guarantee. Documented and tested at both ends.

### D3 — Commit

On a successful LLM response, the use-case calls:

```go
err := wallet.Commit(ctx, wallet.CommitInput{
    ReservationID: resv.ID,
    ActualTokens:  resp.TokensIn + resp.TokensOut,
})
```

The wallet repository runs in one TX:

1. `UPDATE token_wallet SET reserved = reserved - resv.Tokens,
   version = version + 1 WHERE id = $wallet AND version = $expected`
   — release the reservation.
2. `UPDATE token_wallet SET balance = balance - 0` is a no-op:
   the balance was already debited at `Reserve`. Note however
   that `Reserve` debited the *estimate*; we now need to refund
   the difference between estimate and actual.
3. `UPDATE token_wallet SET balance = balance + (resv.Tokens - ActualTokens),
   version = version + 1 WHERE id = $wallet AND version = $expected2`
   — refund the unused portion of the reserve, if any.
4. Insert one ledger row with `direction='commit'`,
   `state='confirmed'`, `tokens=ActualTokens`, `source='ai_assist'`,
   `idempotency_key=sha256(resv.IdempotencyKey || ':commit')`.

The refund step is the AI-specific addition over the per-message
flow: per-message debits know the cost upfront, so reserve == actual
and there is no refund. AI assist's `actual ≤ estimate` invariant
makes the refund a normal, common path (most calls undershoot the
policy cap).

The commit ledger row's idempotency key is derived from the
reservation's key by appending `:commit`. This keeps the commit
row unique while linking it deterministically to the reservation
for audit reconstruction.

### D4 — Rollback

If the LLM call fails — transient timeout, hard provider error,
content block, context cancellation — the use-case calls:

```go
err := wallet.Rollback(ctx, wallet.RollbackInput{
    ReservationID: resv.ID,
    Reason:        rollbackReason(llmErr),
})
```

The wallet repository in one TX:

1. `UPDATE token_wallet SET reserved = reserved - resv.Tokens,
   balance = balance + resv.Tokens, version = version + 1`
   — release the reservation and refund the full estimate.
2. Insert one ledger row with `direction='rollback'`,
   `state='confirmed'`, `tokens=resv.Tokens`, `source='ai_assist'`,
   `reason=$reason`,
   `idempotency_key=sha256(resv.IdempotencyKey || ':rollback')`.

`Rollback` is always safe to retry: the ledger UNIQUE on
`idempotency_key=…:rollback` short-circuits duplicates. If the
process crashes between LLM-error and DB-Rollback, the next
heartbeat or the nightly reconciler will see the stranded
reservation and call `Rollback` to clean it up.

The stranded-reservation cleanup is the nightly wallet
reconciler's job (already specified in [ADR
0088](./0088-wallet-basic.md) §D6): any reservation older than
its TTL with no matching commit/rollback ledger row is rolled
back automatically. The TTL for `source='ai_assist'` reservations
is **2× the LLM timeout budget** (16 seconds — 2× the 8s p99 from
[ADR 0040](./0040-llm-port-retry-idempotency.md) §D3). A
reservation older than 16 seconds is by definition stranded
because no in-flight LLM call should outlive its own timeout.

### D5 — Insufficient balance

`Reserve` fails with `wallet.ErrInsufficientFunds` when
`balance < Tokens`. The use-case maps this to
`aiassist.ErrInsufficientBalance` and:

1. Returns the typed error to the caller. The HTTP handler maps it
   to `402 Payment Required` (or `403`, see operational handbook
   for the choice) and renders a UI message that distinguishes
   "out of tokens" from "service unavailable."
2. Emits a NATS event:

   ```
   Subject: wallet.balance.depleted.v1
   Headers: nats-msg-id = "{tenant_id}:{date(now, day)}"
   Payload: {
     "tenant_id":     "01HXYZ...",
     "subscription_id": "01HXYZ...",
     "depleted_at":   "2026-05-16T19:31:02Z",
     "scope":         "ai_assist",
     "estimated_tokens": 4000,
     "available_tokens": 87
   }
   ```

The NATS event dedupes per-tenant per-day via the `nats-msg-id`
header so we do not flood downstream consumers with one event per
denied call. The same tenant hitting the depleted state ten times
in a day produces one event for the JetStream stream consumers,
plus N denied calls in the audit log.

Downstream consumers of `wallet.balance.depleted.v1` in Fase 3:

- **W3C** (referenced in [SIN-62196](/SIN/issues/SIN-62196)) —
  composes a notification to the tenant admin so they can top up.
  The composer ignores duplicate events (per-day dedup is enough).
- The wallet dashboards subscribe and surface a "tenant out of
  tokens" banner to operators with master access.

The event is on the existing `WALLET_EVENTS` JetStream stream
(per [ADR 0088](./0088-wallet-basic.md) §D5). Subject naming
follows the project convention `wallet.<aggregate>.<action>.v<n>`.

### D6 — Hexagonal boundary

The wallet domain (`internal/wallet/`) already owns the
`Reserve`/`Commit`/`Rollback` aggregate methods (per [ADR
0088](./0088-wallet-basic.md)). This ADR introduces no new domain
shape; it composes existing primitives.

`internal/aiassist/usecase.go` consumes:

- The `Wallet` port (already declared as
  `internal/wallet/port.go`).
- A new `BalanceDepletedPublisher` port for the NATS event:

  ```go
  type BalanceDepletedPublisher interface {
      Publish(ctx context.Context, e BalanceDepleted) error
  }
  ```

  Lives in `internal/aiassist/events.go`. The NATS adapter at
  `internal/adapter/messaging/nats/wallet_depleted_publisher.go` is
  the only file that imports `github.com/nats-io/...` for this
  flow.

The wallet domain core does not import the AI assist package; the
AI assist domain core does not import NATS or `database/sql`.

### D7 — Audit and reconciliation

Every `Reserve`, `Commit`, and `Rollback` writes a ledger row.
The reconciler (per [ADR 0088](./0088-wallet-basic.md) §D6) gains
no new responsibility: stranded `ai_assist` reservations are
covered by the existing stranded-reservation pass with the
shorter TTL from §D4.

The audit-trail invariant per tenant per request:

- A successful AI assist call writes **exactly two** ledger rows
  with the same key prefix: one `reserve`+`pending` and one
  `commit`+`confirmed`. The reservation row is later updated to
  `released` at commit time (per [ADR 0088](./0088-wallet-basic.md)
  §D4) — same row, state transition; ledger never deletes.
- A failed AI assist call writes **exactly two** rows: one
  `reserve`+`pending` and one `rollback`+`confirmed`. The
  reservation row transitions to `released` at rollback.
- A duplicate request (operator retry with same `request_id`)
  writes **the same two rows** — the second `Reserve` returns the
  first reservation; the second `Commit`/`Rollback` is a no-op
  per the `…:commit`/`…:rollback` UNIQUE.

The reconciliation aggregate check (sum of ledger entries ==
wallet balance + reserved) is unchanged from [ADR
0088](./0088-wallet-basic.md). Drift > 1% alerts; AI assist does
not relax this threshold.

## Consequences

Positive:

- Reuses existing wallet primitives (reserve/commit/rollback,
  ledger UNIQUE, version-conditional UPDATE, nightly reconciler).
  Boring technology budget held.
- **Idempotence** at the ledger UNIQUE makes retries safe end-to-
  end. The LLM port's `request_id` stability ([ADR
  0040](./0040-llm-port-retry-idempotency.md) §D4) is the matching
  contract.
- Tenant is never charged for a failed call: `Rollback` refunds
  the full estimate before the error surfaces.
- Tenant is charged only for actual tokens used: the commit
  refunds the (estimate - actual) delta before confirming the
  ledger row.
- Crash-safe: stranded reservations are picked up by the
  reconciler within ~24h; the TTL of 16s for AI assist
  reservations means in practice the next heartbeat or the next
  call from the same tenant fixes them in seconds.
- Insufficient-balance is a first-class error path with a NATS
  event for downstream reaction, dedup'd per-tenant per-day.

Negative / costs:

- The refund step inside `Commit` makes the per-AI-call wallet
  flow slightly more expensive than per-message (two conditional
  UPDATEs vs one). Not a concern at the 8s LLM latency envelope
  but worth noting.
- The 16s TTL for `ai_assist` reservations is shorter than the
  per-message reservation TTL. The reconciler's pass needs the
  source-aware TTL; we add a `source → ttl` map in the reconciler
  config and a unit test that asserts an `ai_assist` reservation
  is reaped at 16s, not at the default 24h.
- The NATS event introduces a new subject. Operators must subscribe
  to `wallet.balance.depleted.v1` to receive the depletion signal;
  documented in the runbook.

Risk residual:

- A provider that returns a different token count than it billed
  (rare but documented in OpenRouter's history) would commit an
  under-debit. Mitigation: the actual-tokens value comes from
  the provider's response and is the same value the provider
  charges us; if they differ, the discrepancy shows up on the
  monthly invoice reconciliation, not silently. Out of scope for
  this ADR; covered by the cost-reconciliation work in Fase 4.
- A bug that resets `request_id` per attempt would defeat dedup
  and produce multiple `Reserve` rows that all succeed (because
  each has a different idempotency key). Mitigation: handler-side
  test asserts the same operator action produces the same
  `request_id` across retries; rate limiter caps the blast radius
  at one denial per second per (tenant, conversation).
- The `wallet.balance.depleted.v1` dedup window is one day. A
  tenant who runs out, tops up, and runs out again the same day
  receives only the first event. Acceptable: the second denial
  still surfaces in the audit log; the topology of the topology
  consumers (operator banner, admin notification) does not benefit
  from multiple-events-per-day for the same tenant.

## Alternatives considered

### Option A — Debit on success, no reserve

Call the LLM first, then `Debit(actual_tokens)` on success.

Rejected because:

- A crash between LLM-OK and DB-OK leaves us paying the provider
  with no record of the spend. The reconciler cannot recover
  what it cannot see (no reservation, no ledger row at all).
- Concurrent calls from the same tenant could collectively over-
  spend: each call sees a positive balance, all proceed, all
  succeed, balance ends negative. The reserve step is the
  serialisation point.
- The wallet aggregate's invariants
  (`reserved ≤ balance`, no overdraw) require the reserve step
  to be enforced. Removing it is removing the invariant.

This is the same anti-pattern [ADR 0088](./0088-wallet-basic.md)
§D2 already rejected for per-message flows (F30 + F37). We reuse
the rejection here.

### Option B — Reserve == actual, charge tenant at end

Skip the worst-case estimate; reserve the actual cost after the
provider responds, in a single TX with the ledger row.

Rejected because:

- Same crash-window as Option A: the LLM has charged us before
  the wallet has any record of the spend. The reconciler still
  has nothing to recover.
- The wallet's serialisation guarantee disappears: two concurrent
  calls both see a positive balance and proceed to invoke the
  provider, then one of them finds the balance is gone and
  fails the post-call reserve. We have already paid OpenRouter
  for the failed call.
- The reserve step exists precisely to make the *provider*
  invocation conditional on having room in the wallet.

### Option C — Cache estimate, debit periodically

Maintain an in-memory "spent this minute" counter; flush to
the wallet every N seconds.

Rejected because:

- Crash window: a worker that dies before flush forgets the
  spend. The tenant gets a free call; we eat the provider charge.
- Multi-instance: two workers each cache their own "spent"
  counter; the wallet sees the merged sum only at flush time.
  Concurrent over-spend is back.
- The latency budget per call (8s) already amortises the
  database round-trip cost. Caching to save a millisecond is not
  worth the failure mode.

### Option D — Single ledger row per call (no reserve)

Issue one `direction='debit'` ledger row at commit time, no
separate reserve row.

Rejected because:

- The reservation row is the audit signal that "the wallet was
  consulted before the provider was called." Without it, the
  forensic question "did we authorise this spend?" has no
  durable answer.
- The reservation is what makes concurrent calls safe (per
  [ADR 0088](./0088-wallet-basic.md) §D2). Removing it is
  removing the protection.

## Lenses cited

- **Idempotence.** Single end-to-end key
  `(tenant_id, conversation_id, request_id)` shared with the
  LLM port. UNIQUE constraints at the ledger enforce
  short-circuit on duplicates.
- **Reversibility & blast radius.** A provider outage produces
  a clean rollback; a wallet outage refuses to authorise the
  spend (deny by default); a crash between provider-OK and
  DB-OK is recovered by the reconciler within 16 seconds.
- **Defence in depth.** Idempotency at three layers (LLM
  provider header, wallet ledger UNIQUE, reconciler stranded-
  reservation sweep). Any two failing leaves the third holding.
- **Hexagonal / ports & adapters.** AI assist consumes the
  existing wallet port; the NATS publisher is a new port with
  one adapter; the wallet domain remains untouched.
- **Boring technology budget.** Reuses every primitive from
  [ADR 0088](./0088-wallet-basic.md). One new ledger `source`
  value, one new NATS subject.
- **Observability before optimisation.** Audit rows per call,
  `aiassist.spend_actual_tokens` and `aiassist.spend_refund_tokens`
  metrics, NATS-event consumer-lag metric. All shipping with the
  feature.

## Out of scope

- Per-call cost caps as a separate ledger entity (the policy's
  `max_tokens_per_call` is the cap today; a future "spend cap
  per hour" would be a new ledger primitive and a new ADR).
- Refunds to the tenant for provider-side discrepancies. Out
  of scope until cost reconciliation lands in Fase 4.
- Pre-buying tokens at a discount (volume pricing). Pricing
  changes are a Fase 4+ concern.
- Streaming responses where tokens accrue progressively. The
  port is single-shot for Fase 3 ([ADR
  0040](./0040-llm-port-retry-idempotency.md) §"out of scope").
