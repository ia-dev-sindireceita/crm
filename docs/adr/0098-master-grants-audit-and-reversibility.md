# ADR 0098 — Master grants: audit trail and reversibility while unconsumed

- Status: Accepted
- Date: 2026-05-16
- Deciders: CTO
- Drives: [SIN-62876](/SIN/issues/SIN-62876) (this ADR), [SIN-62195](/SIN/issues/SIN-62195) (Fase 2.5 parent)
- Builds on: [ADR 0074](./0074-master-mfa-phase0.md) (master TOTP + `mfa_verified_at`),
  [ADR 0088](./0088-wallet-basic.md) §D7 (master grant caps, 4-eyes for over-cap),
  [ADR 0093](./0093-courtesy-grant-on-tenant-creation.md) (initial courtesy grant)

## Context

Fase 2.5 ([SIN-62195](/SIN/issues/SIN-62195)) introduces explicit
master-issued grants beyond the initial courtesy grant covered by
[ADR 0093](./0093-courtesy-grant-on-tenant-creation.md). A master
can now issue two kinds of grant out-of-band of normal subscription
renewals:

- **`free_subscription_period`** — gives the tenant N months of plan
  access without invoice. Materialises as a `subscription` row with
  state `granted` plus a `master_grant` row referencing it.
- **`extra_tokens`** — credits a one-off token quota into the tenant
  wallet. Materialises as a `token_ledger` credit plus a
  `master_grant` row referencing the ledger entry.

Both are privileged actions. The risk model is internal fraud or a
compromised master account: a single master clicking "grant 10M
tokens to tenant-X" with no audit trail is an undetected loss event.
The existing primitives — [ADR 0088](./0088-wallet-basic.md) §D7
(per-grant cap, 4-eyes above 1M tokens) and [ADR
0074](./0074-master-mfa-phase0.md) (TOTP re-verify on
`master.grant_courtesy`) — defend against *unauthorised* grants.
They do not, by themselves, give us a defensible **audit trail** of
who granted what and why, nor a way to **reverse** a grant that
turned out to be wrong while it is still recoverable.

This ADR specifies the audit and reversibility properties every
`master_grant` must have, independent of which grant kind it is.

## Decision

**Every master-issued grant carries an external ULID identifier, a
human-written `reason` of at least 10 characters, a
`created_by_user_id` referencing the specific master user (never a
generic role), and a synchronous audit log entry. A grant is
revocable while `consumed_at IS NULL`; once consumed, only a
compensating grant can offset it. Issuing a grant requires the
session to have a fresh TOTP verification per [ADR
0074](./0074-master-mfa-phase0.md).**

### D1 — Schema: `master_grant`

```sql
CREATE TABLE master_grant (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    external_id         TEXT        NOT NULL UNIQUE,        -- ULID (Crockford base32, 26 chars)
    tenant_id           UUID        NOT NULL REFERENCES tenant(id),
    kind                TEXT        NOT NULL
        CHECK (kind IN ('free_subscription_period', 'extra_tokens')),
    payload             JSONB       NOT NULL,               -- kind-specific (e.g. {"tokens": 100000} or {"months": 1, "plan_id": "..."})
    reason              TEXT        NOT NULL
        CHECK (char_length(reason) >= 10),
    created_by_user_id  UUID        NOT NULL REFERENCES master_user(id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    consumed_at         TIMESTAMPTZ NULL,                   -- when wallet/billing applied the grant
    consumed_ref        TEXT        NULL,                   -- e.g. token_ledger.id or subscription.id
    revoked_at          TIMESTAMPTZ NULL,
    revoked_by_user_id  UUID        NULL REFERENCES master_user(id),
    revoke_reason       TEXT        NULL,

    CONSTRAINT master_grant_revoke_consistency CHECK (
        (revoked_at IS NULL AND revoked_by_user_id IS NULL AND revoke_reason IS NULL)
        OR (revoked_at IS NOT NULL AND revoked_by_user_id IS NOT NULL
            AND revoke_reason IS NOT NULL AND char_length(revoke_reason) >= 10
            AND consumed_at IS NULL)
    )
);

CREATE INDEX master_grant_tenant_idx ON master_grant (tenant_id, created_at DESC);
CREATE INDEX master_grant_unconsumed_idx ON master_grant (tenant_id)
    WHERE consumed_at IS NULL AND revoked_at IS NULL;
```

Notes:

- **`external_id` is a ULID, not the row UUID.** The ULID is what
  appears in audit logs, support tickets, master-UI URLs, and the
  `consumed_ref` of downstream rows. The internal `id` is an
  implementation detail. ULID lexicographic ordering also gives us
  cheap chronological sort without joining `created_at`.
- **`reason` is `NOT NULL` with `CHECK (char_length >= 10)`.** The
  check is intentionally cheap — it prevents the worst class of bad
  audit entry ("ok", "fix", empty string) without forcing a server-
  side validator that could be bypassed by direct SQL. 10 chars is
  enough to force a coherent half-sentence; legitimate reasons are
  almost always longer.
- **`created_by_user_id` references `master_user(id)`, not a role.**
  We must be able to answer "which human did this," not "some
  master did this." Service accounts that issue automatic grants
  (e.g. the courtesy grant of [ADR 0093](./0093-courtesy-grant-on-tenant-creation.md)) get their own dedicated
  `master_user` row tagged `is_system = true` — that grant kind is
  also captured here for auditability, even though it bypasses the
  TOTP requirement (system accounts have no human to verify).
- **`master_grant_revoke_consistency`** enforces: a revoked grant
  must have all three revoke fields populated, a long-enough
  `revoke_reason`, **and** `consumed_at IS NULL`. A consumed grant
  cannot be revoked at the row level; the only recourse is a
  compensating grant (D4).
- **Partial unconsumed-and-unrevoked index** supports the master UI
  listing "pending grants for tenant X" without a full scan.

### D2 — Synchronous audit log via `master.grant.issued`

When a master issues a grant, the handler emits an audit log entry
**in the same transaction** as the `master_grant` INSERT:

```sql
INSERT INTO master_grant (...) VALUES (...);
INSERT INTO audit_log (
    event_type, actor_user_id, tenant_id, resource_kind, resource_id,
    payload, occurred_at
) VALUES (
    'master.grant.issued',
    $1,                                  -- created_by_user_id
    $2,                                  -- tenant_id
    'master_grant',
    $3,                                  -- external_id (ULID)
    jsonb_build_object(
        'kind', $4, 'reason', $5, 'payload', $6
    ),
    now()
);
COMMIT;
```

Synchronous (not async via NATS) because audit-trail durability is
the hard requirement: if the grant landed, the audit landed; if the
audit failed, the grant rolls back. NATS would weaken this with an
eventual-consistency window, which is unacceptable for a
fraud-detection control. The audit row is on the **hot path** of
issuing the grant; we pay one extra `INSERT` per grant, which is
operationally negligible for a low-rate action.

Revocation emits `master.grant.revoked` in the same shape, with
`revoked_by_user_id`, `revoke_reason`, and the original `external_id`
in the payload. Consumption (the wallet credit or subscription
creation) emits `master.grant.consumed` with `consumed_ref` — that
event is written by whichever consumer landed the grant, in its own
transaction, so the audit trail is end-to-end:

```
master.grant.issued   (writes master_grant row)
master.grant.consumed (writes token_ledger or subscription row)
master.grant.revoked  (writes only if revoked before consumption)
```

`audit_log` is already tenant-scoped with RLS per
[ADR 0072](./0072-rls-policies.md); master rows are tenant-less.
Both branches are in scope: tenant-scoped manager users can see
their own tenant's `master.grant.*` events for transparency, masters
see all.

### D3 — Pre-requisite: fresh TOTP verification in session

The HTTP handler for `POST /m/grants` is composed with
`RequireMasterMFA` from [ADR 0074](./0074-master-mfa-phase0.md) **and**
the stricter "re-MFA explicit" requirement from §4 of that ADR — the
handler refuses to proceed unless `session.mfa_verified_at >=
now() - $reMFAWindow` (default 5 minutes).

`free_subscription_period` and `extra_tokens` are both listed in the
re-MFA-required set, joining `master.grant_courtesy`,
`master.impersonate.request`, and `master.feature_flag.write`. The
master UI prompts for a TOTP code immediately before submitting the
grant form when the window has lapsed.

The two layers compose:

1. `RequireMasterMFA` blocks the route unless the session has any
   `mfa_verified_at`.
2. The handler-level check refuses unless `mfa_verified_at` is
   within the re-MFA window.

Both are deny-by-default. A future grant kind added under
`master.grants.*` inherits the route-level block automatically (per
ADR 0074 §3); the handler-level fresh-MFA check is enforced by a
shared `RequireFreshMFA(window)` helper, tested by a convention test
that fails the build if any handler in
`internal/adapter/httpapi/mastergrants/` does not invoke it.

### D4 — Revocability rules

A grant transitions through three terminal-ish states:

| State      | Predicate                                              | Allowed transitions               |
|------------|--------------------------------------------------------|-----------------------------------|
| Pending    | `consumed_at IS NULL AND revoked_at IS NULL`           | → Consumed, → Revoked             |
| Consumed   | `consumed_at IS NOT NULL`                              | (terminal — compensation only)    |
| Revoked    | `revoked_at IS NOT NULL AND consumed_at IS NULL`       | (terminal)                        |

**Revocation while pending** is a normal master operation:

```sql
UPDATE master_grant
   SET revoked_at         = now(),
       revoked_by_user_id = $1,
       revoke_reason      = $2
 WHERE id = $3
   AND consumed_at IS NULL
   AND revoked_at IS NULL
 RETURNING external_id;
```

`0 rows affected` → the grant is no longer pending (either consumed
or already revoked). The CHECK constraint at the schema level
guarantees that a `consumed_at IS NOT NULL` row cannot be revoked
even by direct SQL — defence in depth against bug, malicious
operator, or pg client mishap.

**Consumption** happens when the downstream context applies the
grant:

- `extra_tokens` → the wallet allocator (see [ADR
  0097](./0097-subscription-and-invoice-model.md) D3 idempotency
  pattern, reused) inserts a `token_ledger` credit referencing the
  grant's `external_id` as `idempotency_key`-source, then updates
  `master_grant.consumed_at = now()` and
  `consumed_ref = <ledger external id>` in the **same transaction**.
- `free_subscription_period` → the billing context creates a
  `subscription` row with `state = 'granted'`, then updates the
  grant the same way with `consumed_ref = <subscription external
  id>`.

If consumption races with revocation, the consumer wraps the update
in a conditional UPDATE on `consumed_at IS NULL AND revoked_at IS
NULL`. Only one of the two wins, deterministically — Postgres row
locking serialises them at the moment they touch the same row.

**Partial consumption** — for `extra_tokens` grants, the tenant may
have spent part of the credit before a revoke decision is made. The
ledger captures the credit as a single, atomic row (no partial-credit
concept); once that row is committed, `consumed_at` is set and the
grant is no longer revocable. The corrective path is then:

1. The master issues a **compensating grant** of kind
   `extra_tokens` with a negative-equivalent effect. We do not store
   negative `tokens` directly (the wallet schema rejects that —
   credits are always positive). Instead, the compensating grant is
   modelled as a new `master_grant` row with
   `kind='extra_tokens'`, `payload={"tokens": <positive>,
   "compensates_grant_id": "<original ulid>"}`, and the wallet
   consumer applies it as a `direction='debit'` ledger entry with
   `reason='master_grant_compensation'`.
2. If the wallet balance is below the compensation amount, the
   compensation is capped at the remaining balance (we never push
   the wallet negative — same rule as [ADR
   0097](./0097-subscription-and-invoice-model.md) §D6). The audit
   row records both the intended compensation and the applied
   amount; the difference is documented in `revoke_reason` of the
   compensation grant.

For `free_subscription_period` grants, partial consumption is
month-by-month: the granted subscription's first renewal cycle
commits the first month; revoking after that month requires a
compensating grant that cancels the remaining months. The
compensation path is symmetric to the `cancelled_by_master` flow in
[ADR 0097](./0097-subscription-and-invoice-model.md) §D6.

### D5 — Caps inherited from ADR 0088 §D7

The caps from [ADR 0088](./0088-wallet-basic.md) §D7 apply uniformly
to **both** grant kinds, with `extra_tokens` measured directly in
tokens and `free_subscription_period` measured in
`months × plan.monthly_token_quota`:

| Cap                        | Value                                  | Behaviour above cap                |
|----------------------------|----------------------------------------|------------------------------------|
| Per-grant                  | 10,000,000 tokens-equivalent           | 403, requires 4-eyes approval      |
| Per-master per-365d        | 100,000,000 tokens-equivalent          | 403, requires 4-eyes approval      |
| Alert threshold            | 1,000,000 tokens-equivalent            | Slack `#alerts`, grant proceeds    |

The 4-eyes approval flow from ADR 0088 §D7 — a pending
`MasterGrantRequest` row that requires a second master to confirm —
is the gating mechanism. Implementation detail: a grant above cap
is **not** an immediate `master_grant` row; instead it lives in
`master_grant_request` (separate table) with `state='awaiting_approval'`,
its own `reason`, its own `created_by_user_id`, and a
`requires_second_approver_id IS NULL` slot that the second master
fills. Only when both approvals land does the request promote to a
real `master_grant` row + the audit chain D2 begins.

Both masters' actions (request + approval) emit their own audit
events (`master.grant.requested`, `master.grant.approved`) so the
full trail is durable. The approver cannot be the requester
(enforced at handler and SQL CHECK: `requires_second_approver_id <>
created_by_user_id`).

### D6 — Hexagonal boundary

`internal/master/grants` is the domain core. It declares:

```go
type GrantRepository interface {
    Insert(ctx context.Context, g Grant) (Grant, error)
    Revoke(ctx context.Context, grantID uuid.UUID, by uuid.UUID, reason string) error
    MarkConsumed(ctx context.Context, grantID uuid.UUID, ref string) error
    ListPending(ctx context.Context, tenantID uuid.UUID) ([]Grant, error)
}

type AuditEmitter interface {
    Emit(ctx context.Context, event AuditEvent) error
}
```

The Postgres adapter (`internal/adapter/db/postgres/grants/`) is the
only place that imports `database/sql` / `pgx` for this flow. The
HTTP handler (`internal/adapter/httpapi/mastergrants/`) is the only
place that imports HTTP types. The wallet and billing consumers
that materialise grants do so through their own existing adapters
(per [ADR 0088](./0088-wallet-basic.md) and [ADR
0097](./0097-subscription-and-invoice-model.md)) and call
`MarkConsumed` via the port.

The domain core enforces the invariants that schema constraints
also enforce — duplicated defence-in-depth:

- `reason` length ≥ 10 (validated in the domain before the SQL).
- `revoke_reason` length ≥ 10.
- Cap checks (per-grant + per-master + per-tenant) **before** the
  Postgres roundtrip, so a cap violation returns a clean 403 from
  the domain without exhausting a DB connection.

## Consequences

Positive:

- Every grant is attributable to a specific master user with a
  human-readable reason; "I don't remember why I did that" is
  structurally impossible after the fact.
- Pending grants are reversible without compensation; consumed
  grants are reversible with full audit history. The wallet/billing
  state is always reconstructable from the audit log + the
  `master_grant` history.
- Compromise of a single master account is bounded: cap + 4-eyes
  + fresh TOTP + audit alert form a defence-in-depth stack. To
  silently drain a tenant the attacker needs the master's session,
  fresh TOTP, a second compromised master to sign off (if above
  cap), and no one watching the `#alerts` Slack channel.
- The audit chain (`issued → consumed | revoked`) is the canonical
  source of truth for support tickets ("why does tenant X have N
  extra tokens?") and incident response.
- Schema-level CHECK constraints catch buggy callers and direct-SQL
  mishaps without relying on application code paths.

Negative / costs:

- Per-grant write amplification: one `master_grant` row + one
  `audit_log` row + one `notifier/slack` call above 1M threshold.
  Acceptable: grants are a low-rate operator action, not a hot path.
- Compensation grants for already-consumed cases require explicit
  master action — there's no "undo" button for a consumed grant.
  This is the *right* behaviour (we never silently rewrite ledger
  history) but it does mean operators need to be trained on the
  compensation flow.
- The fresh-TOTP requirement adds friction. Mitigated by the
  re-MFA-required set already including `master.grant_courtesy`
  (ADR 0074); operators are already used to the prompt.
- Caps + 4-eyes flow means a legitimate large grant (e.g. an
  enterprise deal closing) requires both a primary and approving
  master to be reachable. Acceptable for Fase 2.5 master headcount
  (≤ 5 expected first year).

Risk residual:

- A pair of colluding masters could approve each other's
  over-cap grants. Mitigation: the `#alerts` Slack notification
  fires at the **1M alert threshold** below the cap, well before
  4-eyes engages; persistent over-cap pattern is visible to anyone
  with channel access (operators, CEO, on-call). Not a structural
  defence, but a detection control.
- Compromised master with valid TOTP can still issue
  under-cap grants. The audit row names them; the alert at 1M
  bounds the per-grant size attackers can issue without triggering
  detection. Reversibility (D4) limits the durable damage if caught
  before consumption.

## Alternatives considered

### Option A — Asynchronous audit log via NATS

Emit the `master.grant.issued` event via NATS JetStream after the
grant `INSERT` commits, mirroring the billing/wallet pattern from
[ADR 0097](./0097-subscription-and-invoice-model.md).

Rejected because:

- The grant + audit must be atomically consistent. NATS introduces
  a delivery window; an audit emitted late or lost is not a
  recoverable event for fraud detection (the operator is already
  gone by the time the reconciler notices).
- Lens **defence in depth**: synchronous audit at the
  `master_grant` write moment ensures the audit row exists by the
  time any other process — including the master-UI poll that
  surfaces "your grant succeeded" — can observe the grant. There
  is no "the grant happened but the audit didn't" window.
- The grant rate is low enough (single-digit per day at Fase 2.5
  scale) that the extra `INSERT` per grant is not a measurable
  cost.

The async pattern in ADR 0097 is appropriate for billing/wallet
cross-context coordination — different invariants, different SLA.
Audit log on a privileged action is the wrong shape for async.

### Option B — Soft delete via a `state` column

Use a single `state ENUM ('pending', 'consumed', 'revoked')` column
instead of `consumed_at` / `revoked_at` nullable timestamps.

Rejected because:

- We lose the "*when* did this transition" information that the
  audit log uses. We would have to denormalise it into the audit
  payload or join `audit_log` for every grant inspection.
- The CHECK constraint that enforces "consumed grants cannot be
  revoked" is much cleaner with two nullable timestamps and a
  predicate than with a state machine encoded in a column.
- Lens **idiomatic Go**: errors as values, states as predicates.
  An ENUM is more brittle to extend (we already see
  `cancelled_by_master` for invoices in ADR 0097); two timestamps
  + computed state is well-trodden ground.

### Option C — Allow revocation of consumed grants by rewriting ledger

Let a master revoke a consumed `extra_tokens` grant by deleting or
flipping the sign of the `token_ledger` row.

Rejected because:

- Lens **defence in depth**: the ledger is supposed to be
  append-only. Mutating committed ledger rows breaks the
  reconciler invariant (ADR 0088 §D6) — the reconciler assumes
  ledger sum equals wallet balance delta over the same window.
- Audit-trail forensics rely on the ledger being immutable. If
  ledger rows can disappear or change sign, the cryptographic
  chain (Fase 4 if we add one) and the human review process both
  lose their footing.
- The compensation-grant pattern (D4) achieves the same
  *user-visible* outcome (net-zero tokens) while preserving
  history. The customer reads back "we credited 100k, then
  compensated 100k due to ⟨reason⟩" — the trail is explicit and
  auditable.

### Option D — `created_by_user_id` references a generic master role

Store `created_by_role` ('master', 'admin', etc.) instead of a
specific user.

Rejected because:

- The audit trail loses its primary value: attribution. Knowing
  "some master did this" does not survive an investigation.
- The 4-eyes flow (ADR 0088 §D7, lifted into D5 here) requires
  "the approver cannot be the requester" — that constraint is
  meaningless without per-user identity.
- Lens **least privilege**: role-level identity hides which
  individual exercised the privilege, which is the same as
  granting the privilege to all role-holders collectively.

## Lenses cited

- **Defence in depth.** Cap (code) + 4-eyes (workflow) + fresh
  TOTP (session) + synchronous audit (DB) + Slack alert at 1M
  threshold (observability). Five layers, no single point of
  failure.
- **Least privilege.** `created_by_user_id` references a specific
  master, not a role; system-grant accounts have their own
  `is_system = true` master row so even automated grants are
  attributable.
- **Hexagonal / ports & adapters.** Domain core declares
  `GrantRepository` and `AuditEmitter` ports; Postgres adapter,
  HTTP adapter, wallet consumer, billing consumer all sit outside
  the core.
- **Reversibility & blast radius.** Pending grants revoke
  cleanly; consumed grants take a compensation. A bad grant has a
  bounded blast radius (one tenant) and a deterministic recovery
  path.
- **Boring technology budget.** Postgres CHECK constraints,
  audit_log table that already exists, master MFA middleware that
  already exists. Zero new infrastructure.
- **Idiomatic Go.** Errors as values from the domain, context
  propagation, table-driven tests for the cap + revoke + consume
  state machine.

## Rollback

If the grant flow proves too operationally heavy (e.g. the 4-eyes
gating blocks legitimate grants under deadline), the migration path
is:

1. Raise the 4-eyes threshold via an ADR amendment to ADR 0088 §D7
   (the caps are durable security parameters, not feature flags).
2. Lower the fresh-TOTP window from 5 minutes to e.g. 30 minutes
   via a config change, after weighing it against the re-MFA list
   in ADR 0074 §4.
3. The audit + reversibility properties of *this* ADR are
   non-negotiable rollback targets. We would not roll back D1, D2,
   or D4 without superseding the ADR — they are the durable
   correctness controls.

A "disable the grant flow entirely" rollback is achievable via
removing the route from the master UI; the schema and audit chain
remain in place and historical grants stay queryable.

## Out of scope

- **Money refund.** Cancelling a `free_subscription_period` grant
  or compensating an `extra_tokens` grant adjusts the wallet
  ledger; it does not move money. Refund to payment method is a
  Fase 4 concern (PIX/card integration ADR).
- **Tamper-evident audit log.** The audit log is durable but
  not cryptographically chained. A future ADR may add hash-linking
  if the threat model expands to include malicious DBAs.
- **Master-issued grants to *other masters* (e.g. internal credit
  for QA tenants).** Treated like any other grant: the receiving
  tenant happens to be operator-owned. No special path.
- **Bulk grants** (e.g. "credit 10k tokens to all tenants in plan
  X"). Out of Fase 2.5 scope. If introduced later, each row still
  needs the full audit chain.
- **Master deletion / GDPR erasure.** When a master user is
  removed, `master_grant.created_by_user_id` references survive as
  historical fact; per-user attribution is a feature, not PII to
  erase. A GDPR ADR will codify the exception.
