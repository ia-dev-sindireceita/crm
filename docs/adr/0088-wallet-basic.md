# ADR 0088 — Wallet básico: atomic reserve + commit-after-LLM + nightly reconcile

- Status: accepted
- Date: 2026-05-14
- Deciders: CTO
- Tickets: [SIN-62723](/SIN/issues/SIN-62723) (this ADR), [SIN-62193](/SIN/issues/SIN-62193) (Fase 1 parent),
  [SIN-62227](/SIN/issues/SIN-62227) (F30 race + F37 commit-after-LLM + F39 grant cap closures),
  [SIN-62239](/SIN/issues/SIN-62239) / [SIN-62240](/SIN/issues/SIN-62240) (F30/F37 implementation tickets;
  decisions inherited, not reopened),
  [SIN-62220](/SIN/issues/SIN-62220#document-security-review) (origin security review).

## Context

Fase 1 ([SIN-62193](/SIN/issues/SIN-62193)) needs a wallet that:

- Issues an initial `CourtesyGrant` when a tenant is created.
- Debits tokens for each outbound message that consumes an LLM call (even
  when the per-token cost is zero in this phase — the **flow** must work so
  Fase 2 can flip on the price without changing code shape).
- Survives concurrent debits without going negative.
- Survives a crash between "LLM responded" and "wallet committed" without
  silently double-debiting or silently losing the debit.

The SecurityEngineer review on plan rev 2
([F30/F37/F39 bundle in SIN-62227](/SIN/issues/SIN-62227)) closed three
related vectors. The decisions there are **inherited** by this ADR; they are
not reopened. Specifically:

- **F30** — Reserve race. Two concurrent debits each read `balance > 0` and
  both proceed to call OpenRouter; balance ends negative.
- **F37** — Commit-after-LLM failure. LLM returned OK, DB unavailable for
  the commit; tokens consumed but unrecorded.
- **F39** — `master.grant_courtesy` unbounded. A compromised master grants
  10⁹ tokens to a tenant.

The decisions closed in SIN-62227 were:

1. Reserve uses an atomic conditional UPDATE keyed on `wallet.version`
   **before** the LLM call.
2. Commit is a second conditional UPDATE on the same `version`; failures
   trigger retry-with-backoff (3×) then async reconciliation queue.
3. Nightly reconciliation compares `TokenLedger` aggregate against
   `TokenWallet.balance` and alerts on drift > 1%.
4. Master grant caps: 10M tokens per grant (90d subscription), 100M
   cumulative per master per 365d, > 1M alerts, > cap requires 4-eyes
   approval composed with the existing 2-master minimum.

This ADR is the durable record of those decisions for the engineer who
implements the wallet, and the canonical reference for the
hexagonal / ports & adapters boundary that `internal/wallet` enforces.

## Decision

### D1 — Aggregate: `TokenWallet` and `TokenLedger`

`internal/wallet` is the hexagonal domain core. It declares two aggregates
and one port:

```go
// internal/wallet/wallet.go (illustrative — final shape decided at PR review)
type TokenWallet struct {
    TenantID uuid.UUID
    Balance  int64   // available + reserved together — never negative
    Reserved int64   // staged debits not yet committed
    Version  int64   // optimistic concurrency token
}

type TokenLedger struct {
    ID             uuid.UUID
    TenantID       uuid.UUID
    Direction      Direction // credit | debit
    Tokens         int64
    Reason         Reason    // grant | message_outbound | reconcile | refund
    IdempotencyKey [32]byte  // sha256 of operation-scoped fingerprint
    CreatedAt      time.Time
    CommittedAt    *time.Time // NULL during reserve, populated on commit
}

type Repository interface {
    GetByTenant(ctx context.Context, tenantID uuid.UUID) (TokenWallet, error)
    Reserve(ctx context.Context, w TokenWallet, tokens int64, key [32]byte) (TokenLedger, TokenWallet, error)
    Commit(ctx context.Context, ledgerID uuid.UUID, expectedVersion int64) (TokenWallet, error)
    CompensateOrphan(ctx context.Context, ledgerID uuid.UUID) (TokenLedger, error)
}
```

The Postgres adapter (`internal/adapter/db/postgres/wallet_repo.go`) is the
**only** place that imports `database/sql`. Domain handlers (the inbox
consumer that calls the LLM) interact with the port. Lint custom
(`paperclip-lint no-sql-in-domain`) flags any `database/sql` or
`github.com/jackc/pgx` import inside `internal/wallet/` or any package the
domain core depends on.

### D2 — Reserve = atomic conditional UPDATE on `version`, **before** the LLM call

The reserve operation is a single Postgres statement, inside one short
transaction:

```sql
UPDATE token_wallet
   SET reserved = reserved + $tokens,
       version  = version + 1
 WHERE tenant_id = $tenant
   AND (balance - reserved) >= $tokens
   AND version = $expected_version
 RETURNING version, balance, reserved;
```

- `0 rows affected` → reservation failed. Domain returns
  `ErrInsufficientFunds` if `balance - reserved < tokens`, or
  `ErrConcurrentModification` if `version` advanced. The handler retries
  the read-modify-write **at most** 3× with exponential backoff (100ms /
  300ms / 900ms). Beyond 3, return error and surface to the user — the
  wallet is hot enough that the LLM call would race anyway.
- Same transaction inserts a `TokenLedger` row with
  `direction=debit`, `tokens=$tokens`, `reason=message_outbound`,
  `committed_at=NULL`, and `idempotency_key=$key` (see D4 for key
  composition).
- The LLM call happens **after** this transaction commits. By construction,
  if the worker crashes between reserve-commit and LLM call, the reserve is
  durable; the nightly reconciler (D6) detects an orphaned reserve and
  compensates.

> Why optimistic version locking and not `SELECT ... FOR UPDATE` row lock:
> wallet-per-tenant is a narrow hotspot under burst load. A pessimistic
> row lock would serialise every outbound message for that tenant on a
> single DB row, capping per-tenant throughput at the LLM call latency
> (hundreds of ms). Optimistic version locking serialises only the brief
> Postgres roundtrip and lets the LLM calls run in parallel after their
> reserve succeeded. The retry-3× cap absorbs the rare lost race.

### D3 — Commit = conditional UPDATE on same `version`, **after** the LLM call

Once the LLM returns successfully and the inbox consumer is ready to
materialise the message:

```sql
UPDATE token_wallet
   SET balance  = balance - $tokens,
       reserved = reserved - $tokens,
       version  = version + 1
 WHERE tenant_id = $tenant
   AND version   = $expected_version
   AND reserved >= $tokens
 RETURNING version;

UPDATE token_ledger
   SET committed_at = now()
 WHERE id = $ledger_id
   AND committed_at IS NULL;
```

Both updates run in one transaction with the inbox `Message` /
`Conversation` rows (and the `inbound_message_dedup` insert from ADR 0087).
The whole TX commits atomically. If commit fails:

1. Retry up to 3× (same backoff as D2).
2. On final failure, the worker enqueues a `WalletCompensationJob`
   referencing `ledger_id` and ACKs the NATS message — the inbox **does
   not** materialise the `Message`, but the carrier was already 200-ack'd
   per ADR 0087.
3. The nightly reconciler (D6) picks up uncommitted ledger rows and
   either commits them (if `wallet.balance` allows) or compensates with
   a positive ledger entry.

> Why commit-after-LLM and not before: the user-facing contract is "you
> only pay for what you got." Committing the debit before the LLM call
> would charge for failed LLM calls. Reserving before and committing
> after is the textbook two-phase pattern; SIN-62227 made this decision
> explicit.

### D4 — Idempotency key composition

`idempotency_key = sha256(tenant_id || ':' || channel || ':' ||
inbound_message_dedup.channel_external_id || ':' || op)` where `op` is one
of `reserve` or `commit`. Both phases derive the same `key` deterministically
from the inbox event (ADR 0087 `channel_external_id`). This lets the
nightly reconciler match `TokenLedger` rows back to the originating
`Message` (or its absence) without storing a foreign key, and prevents
double-reserve when the inbox consumer worker is replayed.

`TokenLedger` enforces `UNIQUE(idempotency_key)`. A replay attempt that
tries to insert a second ledger row with the same key short-circuits to
"already reserved" without consulting the wallet — the dedup primitive is
the constraint, not application logic. Defense in depth on top of ADR 0087
`inbound_message_dedup`.

### D5 — Reversibility via per-tenant feature flag

`feature.whatsapp.enabled` (per tenant, stored in `tenants` or a config
table — implementation detail) gates the **entire** outbound LLM /
wallet-debit path for that tenant. Flipping it off:

- Stops the inbox consumer from invoking the LLM for that tenant.
- Stops further reserves / commits on that wallet.
- Leaves any in-flight reserved rows in `TokenLedger` to be cleaned by the
  nightly reconciler (D6) — no synchronous rollback.

The flag is the rollback primitive for the wallet path. It composes with
the ADR 0087 flag of the same name (both gate the same WhatsApp end-to-end
path; flipping one off stops materialisation, flipping both off stops the
whole flow).

### D6 — Nightly reconciliation

A cron job runs at 02:00 America/Sao_Paulo daily (configurable). It:

1. Aggregates `TokenLedger` per tenant for the last 24h:
   `SUM(CASE WHEN direction='credit' THEN tokens ELSE -tokens END)` over
   committed rows.
2. Compares against the corresponding `TokenWallet.balance` delta.
3. **Drift detection:** if `|aggregate - balance_delta| / |balance_delta| > 0.01`
   (1%), emit alert `wallet.reconcile_drift` with tenant_id and absolute
   token count.
4. **Orphan reserves:** scans `TokenLedger` for rows with
   `committed_at IS NULL AND created_at < now() - INTERVAL '1 hour'`.
   For each orphan:
   - Attempt to commit if a corresponding `Message` exists with matching
     `idempotency_key` (replay scenario where commit was lost).
   - Otherwise compensate: insert a positive `TokenLedger` row that
     releases the reserve, increment `wallet.version`, log
     `wallet.orphan_compensated` with tenant_id and tokens.
5. **External cross-check (D6.1, optional):** if OpenRouter cost API is
   wired (Fase 2), compare reported provider usage against our committed
   debits per tenant. Alert `wallet.openrouter_drift` if drift > 1%.

The reconciler is `internal/worker/wallet_reconciler.go`, a boring stdlib
`time.Ticker` with `SELECT FOR UPDATE SKIP LOCKED` semantics for the
orphan scan. It does not use distributed locks or external coordination.

### D7 — Master grant caps (F39)

Master-level grants of courtesy tokens are bounded by **three** caps:

| Cap | Value | Behaviour above cap |
| --- | --- | --- |
| Per-grant | 10,000,000 tokens / 90d subscription | 403, message `"requires approval"` |
| Per-master per-365d cumulative | 100,000,000 tokens | 403, message `"requires approval"` |
| Alert threshold | 1,000,000 tokens | Slack `#alerts`, grant proceeds |

Above any 403 cap, the grant requires **4-eyes approval** by another
master (composed with the existing 2-master minimum from F6 — both
masters must approve, neither can be the requester). Implementation: a
pending `MasterGrantRequest` row, with approval recorded as a separate
row referencing a different `master_id`. The grant only fires when the
approval row lands.

Cap values are stored in config (`config/wallet.go` constants for Fase
1; promoted to a runtime config table when the master UI lands). Cap
**changes** require an ADR amendment — they are durable security
parameters, not feature flags.

### D8 — What this ADR does **not** decide

- **Marketplace, packages, top-ups.** Fase 1 has only `CourtesyGrant`.
  Pricing per-1k-tokens, billing integration, top-up purchases are Fase
  2+ and get their own ADR.
- **Per-message LLM cost model.** Fase 1 uses `tokens = LLM-reported
  prompt_tokens + completion_tokens` summed at debit time. Token
  conversion to currency is out of scope.
- **Wallet observability dashboard.** A follow-up ticket creates the
  Grafana dashboard (reserve rate, commit success rate, reconcile drift,
  orphan rate per tenant). The ADR mandates the metrics; the
  implementation issue specifies the panels.

## Consequences

Positive:

- F30 closed by the conditional UPDATE on `version`: two concurrent
  reserves cannot both succeed because the second sees a stale `version`
  and gets 0 rows affected.
- F37 closed by reserve-before-LLM + commit-after-LLM + nightly
  reconcile: a crash between LLM and commit leaves an orphan reserve
  that the reconciler resolves within ≤ 24h, and the inbox does not
  materialise the `Message` until commit succeeds.
- F39 closed by hard caps in code + 4-eyes approval flow. Compromised
  master can grant at most one cap-sized batch before the second master
  refuses or alerting catches it.
- Hexagonal boundary makes the wallet trivially unit-testable: the
  domain core's `Reserve`/`Commit` are pure logic against the `Repository`
  port, and the Postgres adapter is integration-tested separately.
- Optimistic `version` lock preserves per-tenant throughput; pessimistic
  alternative would have capped throughput at LLM latency.

Negative / costs:

- Reserve and commit are **two** DB transactions instead of one, with
  the LLM call between them. p95 wallet path latency = reserve TX
  (~5ms) + LLM (~hundreds of ms) + commit TX (~5ms) + inbox TX (~10ms).
  Acceptable for messaging; not acceptable for hot-path UI which doesn't
  exist in Fase 1.
- Reconciler is one more worker to monitor. Boring code, but it has to
  exist.
- The 4-eyes flow adds friction to legitimate master grants. Mitigation:
  alerts fire well below the cap (1M threshold) so masters know when
  they're approaching the boundary in routine operation.

Risk residual:

- Drift in the nightly reconciler beyond 1% but caught by alert →
  manual op to investigate within next-business-day SLA. Acceptable
  given Fase 1 scale (tens of tenants).
- LLM provider returns cost in a unit we don't yet support (e.g., model
  switches to per-second billing). The ADR pins tokens-as-unit; provider
  changes need an ADR amendment.

## Alternatives considered

### Option B — Pessimistic row lock (`SELECT FOR UPDATE`) for reserve

Use `SELECT ... FOR UPDATE` on `token_wallet` row during reserve.

Rejected because:

- Serialises every outbound message for the same tenant on a single
  Postgres row lock held until the reserve TX commits. Per-tenant
  throughput is bounded by the LLM-roundtrip-free wallet TX latency.
- Lens **boring technology budget.** Pessimistic locking is a textbook
  solution but unnecessary here — optimistic with 3× retry achieves the
  same correctness with better latency.
- The race window for F30 is the gap between read and update.
  Conditional UPDATE closes that window without holding any lock.

### Option C — Commit debit **before** LLM call

Debit the wallet first, then call the LLM. If the LLM fails, issue a
compensating credit.

Rejected because:

- The user-facing contract is "pay for what you got." Charging before
  the LLM responded creates a window where a brief LLM outage charges
  every queued outbound message. Compensation hides the charge from
  the wallet ledger but does not refund the *latency cost* — the
  customer sees their balance dip and recover, which is worse UX than
  "no charge happened."
- Lens **reversibility.** Compensating credit assumes the failure was
  detected. If our worker crashes after debit-commit and before noticing
  the LLM failure, the compensation never fires. The orphan-detection
  loop in D6 cannot tell a "successful debit waiting for LLM" from a
  "debit that should have been refunded because LLM failed." We would
  need to store the LLM response *before* deciding to commit, which is
  exactly what reserve-then-commit already does.

### Option D — Postgres advisory lock keyed on `tenant_id`

`pg_advisory_xact_lock(hashtext('wallet:' || tenant_id))` at reserve.

Rejected because:

- Same per-tenant serialisation cost as Option B.
- Lens **boring technology budget.** Advisory locks are a stdlib-of-Postgres
  feature engineers reach for when row locks aren't expressive enough;
  here they are not even needed. One more concept to teach future
  reviewers.
- Two-phase commit + version is more portable. If we ever shard the
  wallet table or move it off Postgres for any reason (we will not in
  Fase 1, but ADR durability matters), version-based locking ports;
  advisory locks do not.

### Option E — Marketplace / packages from day one

Implement the marketplace and per-message pricing surface in Fase 1.

Rejected by [SIN-62193](/SIN/issues/SIN-62193) scope. Not reopened here.

## Lenses cited

- **Hexagonal / ports & adapters.** `internal/wallet` declares the
  `Repository` port. `internal/adapter/db/postgres/wallet_repo.go`
  implements it. Domain core does not import `database/sql`.
- **Idiomatic Go.** Conditional UPDATE with `RETURNING`, retries with
  bounded backoff, `context.Context` propagation, errors as values.
- **Boring technology budget.** Postgres optimistic `version` column,
  no Redis, no distributed locks, no two-phase-commit coordinator.
- **Reversibility & blast radius.** `feature.whatsapp.enabled` per
  tenant is the rollback primitive. Cap values in code prevent runaway
  grants even if the UI layer is compromised.
- **Defense in depth.** Reserve+commit two-phase + `UNIQUE` on
  `idempotency_key` + nightly reconcile + OpenRouter cross-check
  (Fase 2) — four layers against the F30/F37 failure modes.
- **Least privilege.** `master.grant_courtesy` capped at code level,
  bypass requires 4-eyes.

## Rollback

If the optimistic-version design turns out to thrash under load (e.g.,
a tenant burst that exceeds 3× retry consistently), the migration path
is:

1. Flip `feature.whatsapp.enabled = false` for the affected tenant —
   immediate stop to the wallet path.
2. Add an ADR amendment that adopts Option B (`SELECT FOR UPDATE`) and
   ship the change with `feature.wallet.pessimistic = true` flag.
3. Flip the tenant flag back on once the schema change is in place.

A rollback to "commit before LLM" (Option C) is not supported — that
path was rejected for durable user-experience reasons.

## Out of scope

- Marketplace, packages, top-ups — Fase 2+ ADR.
- Currency-side billing — separate ADR when billing integration lands.
- Wallet UI for tenant operators — separate UI ADR.
- LLM provider abstraction — `internal/llm` ADR (not yet written, not
  needed for Fase 1 because only OpenRouter exists).
