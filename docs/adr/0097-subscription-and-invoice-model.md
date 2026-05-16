# ADR 0097 — Subscription + Invoice model: event-driven wallet allocation

- Status: Accepted
- Date: 2026-05-16
- Deciders: CTO
- Drives: [SIN-62876](/SIN/issues/SIN-62876) (this ADR), [SIN-62195](/SIN/issues/SIN-62195) (Fase 2.5 parent)
- Builds on: [ADR 0088](./0088-wallet-basic.md) (wallet aggregate, ledger, reconciler)

## Context

Fase 2.5 ([SIN-62195](/SIN/issues/SIN-62195)) introduces real subscriptions:
each tenant has a `Subscription` to a `Plan`, an `Invoice` per billing
period, and a fixed monthly token quota. The existing wallet ([ADR
0088](./0088-wallet-basic.md)) already debits tokens per outbound message
and tracks balance via `TokenWallet` + `TokenLedger`, but it does **not**
know how to credit a periodic monthly allocation tied to subscription
renewal.

Two bounded contexts now exist:

- `internal/billing` — owns `Plan`, `Subscription`, `Invoice`,
  `CourtesyGrant`. Decides when a subscription period rolls over,
  generates the invoice, and tracks payment state.
- `internal/wallet` — owns `TokenWallet`, `TokenLedger`,
  `MasterGrant`, and the monthly allocator. Knows nothing about
  subscriptions or money.

The naive coupling options are tempting and wrong:

- **Option A — wallet reads subscription.** `internal/wallet` imports
  `internal/billing` to ask "is this tenant's subscription active? what
  is its monthly quota?" — pollutes the wallet domain core with billing
  concepts and breaks the hexagonal boundary established in [ADR
  0088](./0088-wallet-basic.md).
- **Option B — billing calls wallet synchronously.** The billing
  renewer calls `wallet.Allocate(tenantID, quota)` directly during the
  same transaction as `invoice.Create`. This couples invoice durability
  to wallet durability — if the wallet TX fails, do we roll back the
  invoice? If we don't, the customer is invoiced for a period they
  cannot use; if we do, the renewer can deadlock against the
  reconciler.

Neither preserves the lens **hexagonal / ports & adapters**, and
neither is reversible: a bug in wallet allocation that takes the wallet
TX down also takes the invoice generator down.

NATS JetStream is already wired in the project
(`internal/adapter/messaging/nats/`); using it for the cross-context
hand-off costs nothing new in operational surface.

## Decision

**`internal/billing` emits `subscription.renewed` on NATS JetStream;
`internal/wallet` consumes the event and credits the monthly token
quota. There is no synchronous cross-context call. Idempotency is
enforced at two layers: the NATS message ID and a partial UNIQUE index
on the `invoice` table.**

### D1 — Event contract

Subject: `billing.subscription.renewed.v1`. JetStream stream:
`BILLING_EVENTS`. Delivery: `JS_ACK_EXPLICIT`, max-deliver 5,
ack-wait 30s.

Payload (JSON, fields stable for v1):

```json
{
  "subscription_id": "01HXYZ...",
  "tenant_id": "01HXYZ...",
  "plan_id": "01HXYZ...",
  "period_start": "2026-06-01T00:00:00Z",
  "period_end":   "2026-07-01T00:00:00Z",
  "monthly_token_quota": 500000
}
```

`nats-msg-id` header: `"{subscription_id}:{period_start_iso}"`. NATS
JetStream dedupes by this header inside the stream's dedup window (set
to 14 days — wider than any plausible operator-replay window for a
monthly event).

The billing renewer publishes **after** the `invoice` row is committed
in Postgres. The publish is best-effort; the redrive path is the
nightly reconciler (D4), not a transactional outbox. See "Alternatives
considered" for why the outbox was rejected.

### D2 — Invoice idempotency at the database

`internal/billing` writes one `invoice` row per subscription period.
The table carries:

```sql
CREATE UNIQUE INDEX invoice_period_unique
    ON invoice (tenant_id, period_start)
 WHERE state <> 'cancelled_by_master';
```

The partial predicate is deliberate: a master cancellation
(see [ADR 0098](./0098-master-grants-audit-and-reversibility.md))
voids the invoice and frees the period for a new invoice (e.g. plan
upgrade mid-cycle). Active invoices are unique per `(tenant_id,
period_start)` and the renewer's `INSERT ... ON CONFLICT DO NOTHING`
short-circuits on retry.

The invoice row is the **source of truth** for "a period happened."
The NATS event is a derivative — losing it does not lose money; the
reconciler recreates the allocation.

### D3 — Wallet consumer idempotency

`internal/wallet` consumes `billing.subscription.renewed.v1` with a
durable consumer named `wallet-monthly-allocator`. On each delivery:

1. Compute the ledger idempotency key:
   `sha256(subscription_id || ':' || period_start_iso || ':' || "alloc")`.
2. Open a Postgres TX.
3. `INSERT INTO token_ledger (..., idempotency_key, direction='credit',
   reason='subscription_renewal', tokens=$quota)` —
   the `UNIQUE(idempotency_key)` constraint from [ADR
   0088](./0088-wallet-basic.md) §D4 short-circuits the duplicate.
4. Conditional `UPDATE token_wallet SET balance = balance + $quota,
   version = version + 1 WHERE tenant_id = $tenant AND version =
   $expected` — retry-3× on `0 rows affected`, identical pattern to
   [ADR 0088](./0088-wallet-basic.md) §D2.
5. Commit. `Ack` the JetStream message **after** the TX commit.

Loss modes:

- **Worker crash between TX commit and Ack** — JetStream redelivers,
  step 3 short-circuits on idempotency_key, the wallet UPDATE is a
  no-op because `version` already advanced. Idempotent.
- **Worker crash between Ack and TX commit** — impossible because TX
  commit precedes Ack.
- **Wallet TX fails after retries** — JetStream redrives up to
  max-deliver (5) with 30s ack-wait, total ≤ 150s. After that the
  message lands on the DLQ `billing.subscription.renewed.dlq`; the
  reconciler (D4) is the durable fallback.

### D4 — Reconciler fallback

The nightly wallet reconciler (already specified in [ADR
0088](./0088-wallet-basic.md) §D6) gains one additional pass:

- Scan `invoice` for rows with `state IN ('pending', 'paid')` and
  `period_start <= now()`, joined left-anti against `token_ledger`
  with matching `idempotency_key`. For each missing row, perform the
  D3 sequence directly (no NATS round trip).
- Emit alert `wallet.subscription_allocation_drift` with tenant_id,
  subscription_id, and the missing period. The alert is informational
  unless drift persists across two consecutive runs (suggests a NATS
  publish bug, not a transient consumer outage).

The reconciler is the durable consistency guarantee. NATS is the
fast path; the reconciler is the safety net.

### D5 — SLA for eventual consistency

The contract between billing and wallet is:

> When `invoice.state` becomes `pending` or later, the corresponding
> wallet allocation lands within **30 seconds** under healthy NATS
> JetStream and worker conditions, or within **24 hours** when the
> NATS fast path is degraded (reconciler catches it on the next
> nightly run).

UI surfaces that show "remaining tokens" after a renewal must tolerate
this window — typically by showing the wallet number with a soft hint
"updates within a minute of renewal" rather than promising a
synchronous read after `POST /invoices`.

Endpoints that **debit** the wallet (outbound messaging) do not
inherit this SLA: they read the wallet directly. A debit that happens
before allocation lands simply sees the pre-allocation balance — the
tenant is bounded to their prior month's leftover until the allocator
catches up. This is the correct behaviour: we do not pre-credit on the
invoice's promise alone.

### D6 — Cancellation and refunds

When `invoice.state` transitions to `cancelled_by_master`:

1. The active partial UNIQUE index releases the period.
2. The billing context emits `billing.subscription.cancelled.v1` with
   the same `subscription_id` and `period_start`.
3. The wallet consumer issues a compensating `token_ledger` row
   (`direction='debit'`, `reason='subscription_cancellation'`,
   `tokens=$quota_remaining`) and decrements `token_wallet.balance`
   accordingly.

`$quota_remaining` = `min(remaining_balance, allocated_quota)`. If the
tenant already spent the quota, the compensation is capped at the
current balance — we do not push the wallet negative. The audit trail
in `token_ledger` records both the original allocation and the
compensation; the reconciler accepts this as the canonical history.

A separate ADR will cover the refund-to-payment-method side of
cancellations when the payment integration lands (Fase 4 PIX). For
Fase 2.5, cancellation is a master-driven operational tool and money
movement is out of scope.

### D7 — Hexagonal boundary

`internal/billing/port.go` declares a single outbound port:

```go
type SubscriptionEventPublisher interface {
    PublishRenewed(ctx context.Context, e SubscriptionRenewed) error
    PublishCancelled(ctx context.Context, e SubscriptionCancelled) error
}
```

The NATS adapter lives at
`internal/adapter/messaging/nats/billing_publisher.go` and is the only
place that imports `github.com/nats-io/nats.go` for this flow.

`internal/wallet/port.go` declares the inbound port:

```go
type SubscriptionEventConsumer interface {
    OnRenewed(ctx context.Context, e SubscriptionRenewed) error
    OnCancelled(ctx context.Context, e SubscriptionCancelled) error
}
```

The NATS consumer adapter at
`internal/adapter/messaging/nats/wallet_consumer.go` is the only place
that wires JetStream into the wallet path. The wallet domain core
remains free of NATS, HTTP, and SQL imports (paperclip-lint
`no-sql-in-domain` and `no-transport-in-domain` enforce this).

The shared event types (`SubscriptionRenewed`, `SubscriptionCancelled`)
live in `internal/billing/events/` and are consumed by both sides via
import; they are pure value types with no behaviour.

## Consequences

Positive:

- Bounded contexts evolve independently. Wallet allocation logic can
  change without redeploying billing. Billing's renewer can change
  cadence without redeploying wallet.
- Each side is unit-testable with its port mocked. Wallet tests do not
  need a real billing DB; billing tests do not need a wallet.
- Replay safety: rerunning the renewer (operational tool) emits the
  same event with the same `nats-msg-id`; JetStream dedupes, and even
  if it didn't, the wallet `idempotency_key` would short-circuit.
- Reversibility: a wallet bug that drops the consumer does not stop
  invoicing. The reconciler catches up the next night. The blast
  radius is "tenant sees stale balance for ≤ 24h," not "all renewals
  fail."
- Reuse of existing primitives: ledger idempotency key, conditional
  version UPDATE, nightly reconciler. No new patterns to learn.

Negative / costs:

- Eventual consistency window of up to 30s under healthy operation.
  Operators must tolerate this in dashboards and support tooling.
- Two reconciler passes to monitor instead of one (the existing wallet
  drift check from ADR 0088, plus the new subscription-allocation
  drift). Boring code, but observability needs both alerts wired.
- DLQ requires manual triage when the wallet TX persistently fails.
  Mitigated by the reconciler picking up missed allocations
  automatically; the DLQ is for genuinely broken events (malformed
  payload, schema drift).

Risk residual:

- A bug in `internal/billing/events` that changes the JSON shape
  without a versioned subject would break the wallet consumer
  silently. Mitigation: subject is versioned (`...renewed.v1`);
  schema changes require a new subject (`...renewed.v2`) and a
  dual-consume window. Enforce in code review.
- NATS JetStream dedup window is finite (14 days). An operator who
  manually replays a 6-month-old event would bypass NATS dedup. The
  wallet `UNIQUE(idempotency_key)` still catches it. Defense in
  depth holds.

## Alternatives considered

### Option A — Synchronous wallet call from billing renewer

The billing renewer calls `wallet.AllocateMonthly(ctx, tenantID,
quota)` directly inside the invoice-creation transaction.

Rejected because:

- Couples invoice durability to wallet durability. A wallet outage
  stalls invoice generation; a wallet bug rolls back invoices and
  causes billing-side state divergence.
- Breaks **hexagonal boundary**: `internal/billing` would need to
  import `internal/wallet` or share a transaction manager. Either
  pollutes the boundary established in ADR 0088.
- Hard to test in isolation: wallet unit tests would need a fake
  billing TX manager, billing unit tests would need a fake wallet
  port. The mocks become the contract instead of an event schema.
- No reversibility: a misbehaving allocator that double-credits has
  no replay-safe rollback because the call site is inside the same
  TX that committed the invoice.

### Option B — Transactional outbox

`internal/billing` writes the `subscription.renewed` event as a row in
an `outbox` table inside the same TX as `invoice`. A relay polls
the outbox and publishes to NATS, marking rows `published`.

Rejected because:

- Adds a new infrastructure pattern (outbox table, relay process,
  poll loop, claim-and-publish semantics) for a single producer that
  already has a durable source of truth: the `invoice` row itself.
- The reconciler (D4) achieves the same durability guarantee by
  reading `invoice` directly. The outbox would be a redundant log of
  the same fact.
- Lens **boring technology budget**: we already use NATS JetStream
  with `nats-msg-id` for dedup and a nightly reconciler for drift.
  Adding an outbox is one more concept and one more failure mode
  (relay liveness) without buying us anything the current design
  doesn't already give.
- Lens **reversibility**: the outbox introduces an
  in-between-state row that has to be reconciled if the relay
  crashes mid-claim. The invoice row, by contrast, is terminal —
  it's already the canonical state.

The outbox would be worth revisiting if we ever needed
strict-ordered, lossless event delivery (e.g. a CDC pipeline
downstream that cannot tolerate the 24h reconciler window). Fase 2.5
does not.

### Option C — Shared transaction across contexts via two-phase commit

XA-style 2PC between billing's Postgres and wallet's Postgres (or a
shared cluster with a coordinated TX).

Rejected because:

- Postgres does not support XA across logical databases without an
  external coordinator. Both contexts live in the same cluster today
  (see [ADR 0071](./0071-postgres-roles.md)), so a shared TX would
  collapse the boundary entirely.
- Lens **defence in depth**: a single transactional failure mode
  would take down both contexts. The asynchronous design isolates
  them.
- Operationally complex: distributed-TX coordinators are notorious
  failure modes. Not justified for a monthly cadence event.

### Option D — Wallet reads invoice directly

The wallet allocator polls the `invoice` table for "rows I haven't
allocated for yet" instead of consuming an event.

Rejected because:

- `internal/wallet` would need to import `internal/billing` schema
  knowledge (table name, column names, state machine). Same boundary
  violation as Option A in a different form.
- The event-driven path makes the contract explicit: wallet depends
  on a **published event**, not on billing's internal table shape.
  Billing can refactor `invoice` to multiple tables tomorrow and as
  long as the event still publishes, wallet doesn't notice.
- Polling adds a steady-state read load proportional to the number
  of subscriptions. NATS push is O(1) per renewal.

The reconciler (D4) does perform a controlled cross-context read,
but as a fallback — once per night, only when the event path
degraded. The steady-state hot path stays event-driven.

## Lenses cited

- **Hexagonal / ports & adapters.** Billing and wallet each declare
  one port; the NATS adapter is the only crossing point. Neither
  domain core imports the other.
- **Domain-driven design (lite).** Two bounded contexts, distinct
  ubiquitous language (subscription/period/invoice vs.
  wallet/ledger/allocation). The event payload is the published
  contract between them.
- **Boring technology budget.** NATS JetStream already in the stack,
  Postgres partial UNIQUE index already in the toolbox, nightly
  reconciler already operational. Zero new infrastructure.
- **Reversibility & blast radius.** A wallet outage does not stop
  invoicing; a billing bug does not corrupt wallet balances; a
  consumer bug is caught by the nightly reconciler. Each failure is
  contained to one side and self-heals within ≤ 24h.
- **Defence in depth.** Three idempotency layers: NATS
  `nats-msg-id` dedup, invoice partial UNIQUE, ledger
  `UNIQUE(idempotency_key)`. Any two can fail and the third still
  prevents double-allocation.
- **Observability before optimisation.** Allocation drift alert,
  DLQ depth metric, consumer-lag metric — all required before the
  flow ships. The reconciler pass logs explicit alerts when it
  catches missed allocations.

## Rollback

If the event-driven path turns out to be too lossy in practice
(e.g. JetStream operational incidents recur), the migration path is:

1. Disable the NATS consumer (`wallet-monthly-allocator` consumer
   paused via JetStream admin). The reconciler continues to run and
   absorbs the entire allocation load on its nightly schedule.
2. Tenants experience up to 24h allocation lag at month roll-over.
   Acceptable as an emergency-mode fallback while the NATS issue is
   investigated.
3. If reconciler-only operation becomes the steady state, write a
   superseding ADR that adopts a synchronous wallet allocator inside
   the billing renewer, behind a feature flag.

The reverse — re-enabling NATS after a degraded period — is just
unpausing the consumer; idempotency keys ensure no double-allocation.

## Out of scope

- Payment integration (PIX, card, boleto). Invoice `state =
  'paid'` is set manually by a master in Fase 2.5; automatic
  payment lands in Fase 4 with its own ADR.
- Refunds to payment methods. Master cancellation issues a wallet
  compensation here; money refund is out of scope until payment
  integration exists.
- Per-tenant plan upgrades mid-cycle. Fase 2.5 allows
  cancellation + reissue; mid-cycle prorate is a Fase 3+ concern.
- Master-issued discretionary grants (`extra_tokens`,
  `free_subscription_period`). Those are covered by [ADR
  0098](./0098-master-grants-audit-and-reversibility.md).
