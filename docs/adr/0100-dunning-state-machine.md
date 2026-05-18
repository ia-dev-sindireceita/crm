- Status: accepted
- Date: 2026-05-17
- Deciders: CTO
- Ratifies: board decision **D1** in [SIN-62204](/SIN/issues/SIN-62204) (2026-05-08)
- Drives: [SIN-62967](/SIN/issues/SIN-62967) (this ADR), [SIN-62957](/SIN/issues/SIN-62957) (shipped domain in PR #176, commit `6b3012a`)
- Builds on: [ADR 0093](./0093-courtesy-grant-on-tenant-creation.md) (CourtesyGrant kinds), [ADR 0097](./0097-subscription-and-invoice-model.md) (Subscription/Invoice schema)
- Related: [ADR 0086](./0086-fork-only-migration-numbering.md) (fork-only migration numbering — explains why this ADR is **0100** rather than the originally-issued `0086` index)

> **Numbering note.** [SIN-62204](/SIN/issues/SIN-62204) was originally
> filed against `ADR-0086`. After the fork reset ([ADR 0085](./0085-fork-upstream-reconcile.md)),
> ADR slot 0086 was reclaimed by the fork-only migration numbering convention
> (CTO ruling 2026-05-13, [SIN-62534](/SIN/issues/SIN-62534)). Per the
> [README](./README.md) rule "numbers are permanent — supersedes/amendments
> live inside the affected ADR, never via renumbering", this ADR takes the
> next free index above [ADR 0099](./0099-funnel-rules-cascade.md). The
> never-merged draft branch `docs/sin-62204-adr-0086-dunning` is superseded
> by this file.

# ADR 0100 — Dunning state machine: per-subscription aggregate, {current,warn,suspended_outbound,suspended_full,cancelled} with default policy {1,7,30,90} and CourtesyGrant override

## Context

Fase 4 ([SIN-62197](/SIN/issues/SIN-62197)) introduces native PIX billing.
Each tenant has a monthly subscription (plan + add-on packages) invoiced via
PIX. When an invoice is not settled by its `due_date`, the subscription is
**past-due** and the platform needs an explicit, audited policy for that
state — both to protect variable COGS (every outbound message consumes paid
channel + LLM tokens) and to give the operator a fair chance to regularise
before the tenant is locked out.

Two alternatives were presented by the CTO in [SIN-62204](/SIN/issues/SIN-62204):

- **(a) Bloqueio escalonado** — warning banner from D+1, outbound block from
  D+7, read-only from D+30, auto-cancel at D+90.
- **(b) Alerta apenas** — permanent banner + daily email, no technical
  suspension.

The board ratified **(a)** on 2026-05-08 with `{1, 7, 30, 90}` as the
default schedule and an administrative override via
`CourtesyGrant.kind=free_subscription_period`. The implementation
([SIN-62957](/SIN/issues/SIN-62957), PR #176, commit `6b3012a`) shipped the
pure-domain state machine and ports in [`internal/billing/dunning/`](../../internal/billing/dunning).
This ADR records the decision, the implemented shape (which is slightly
different from the draft circulated in the original `0086` branch), and the
trade-offs that remain open for downstream phases.

## Decision

Adopt **opção (a) — bloqueio escalonado** as a per-subscription aggregate
state machine. Concretely:

### 1. Aggregate and state set

Dunning is a first-class aggregate root rooted at the **subscription** (one
row per subscription, `UNIQUE (subscription_id)`), not a free-text status
field on the subscription itself. The aggregate is implemented as
`dunning.DunningState` in
[`internal/billing/dunning/dunning.go`](../../internal/billing/dunning/dunning.go)
and persisted by migration
[`0102_phase4_marketing_billing_dunning.up.sql`](../../migrations/0102_phase4_marketing_billing_dunning.up.sql)
in the table `subscription_dunning_states`.

The state set is the closed enum below, mirrored exactly between
[`dunning.State`](../../internal/billing/dunning/state.go) (Go constants)
and the migration's CHECK constraint
(`CHECK (state IN ('current','warn','suspended_outbound','suspended_full','cancelled'))`):

| State                | Severity | Default trigger | Functional effect                                                                                                                  | UI effect                                       |
| -------------------- | -------: | --------------- | ---------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------- |
| `current`            | 0        | invoice paid    | Baseline — no restriction.                                                                                                          | No banner.                                       |
| `warn`               | 1        | D+1             | None — all operations continue.                                                                                                     | Yellow banner + daily email.                     |
| `suspended_outbound` | 2        | D+7             | Outbound (channel sends, LLM-driven automations, outbound webhooks) blocked at the gateway. Inbound continues.                       | Orange banner + daily email + explicit lock UX.  |
| `suspended_full`     | 3        | D+30            | Outbound blocked **and** writes to critical resources (campaigns, pipelines, settings) blocked. Reads remain available.              | Red banner + daily email + dashboard lock.       |
| `cancelled`          | 4        | D+90            | Terminal in the dunning domain. No further escalation, no override accepted. Reactivation requires a fresh subscription elsewhere.   | Red lock; subscription-creation CTA in master.   |

State names diverge from the draft circulated against the legacy `0086`
index (which proposed `past_due_warn`/`past_due_outbound_blocked`/`past_due_readonly`):
the shipped names are flatter, do not collide with the subscription's
financial status field, and match the aggregate-root model where the
dunning row is independent of `subscription.status`. The semantic mapping
to D1 is identical (D+1 banner, D+7 outbound stop, D+30 read-only, D+90
cancel).

### 2. Policy and configurability

Per-plan policy lives in
[`dunning.Policy`](../../internal/billing/dunning/policy.go) with four
strictly-increasing positive thresholds (`WarnDays < OutboundBlockDays <
ReadonlyDays < CancelDays`). The default — `dunning.DefaultPolicy` — is
`{1, 7, 30, 90}`, matching D1.

Plans MAY override individual thresholds without a migration (enterprise
plans can grant `{3, 14, 45, 120}`; free-trial plans may shorten).
`Policy.Validate()` rejects any policy that is non-positive or not strictly
increasing, so an invalid plan row cannot put the cron into an ambiguous
target state.

`Policy.StateForDaysPastDue(days)` is the inclusive resolver: at exactly
`WarnDays` the target is `warn`, at exactly `CancelDays` the target is
`cancelled`. Escalation never downgrades; downgrade is `MarkPaid`'s job
(see §3 below).

### 3. Transitions

`DunningState.Escalate(now, policy, invoiceID, dueDate, override)` is the
only path that walks the row forward. Rules (matching D1 / [SIN-62204](/SIN/issues/SIN-62204)):

- `StateCancelled` is terminal — `Escalate` is a no-op and returns
  `(false, nil)`.
- If `override != nil && override.Until > now`, no-op. The override pauses
  escalation; the cron resumes after the override expires.
- Otherwise compute `target = policy.StateForDaysPastDue(daysSince)` where
  `daysSince = floor((now - dueDate) / 24h)` clamped to zero.
- If `target.Severity() > current.Severity()`, transition (and require a
  non-`uuid.Nil` `invoiceID` so the audit trail always names the driving
  invoice). Otherwise no-op.

`DunningState.MarkPaid(now)` is the **only** downgrade path: it returns the
row to `current`, clears `last_invoice_id` and any active override, and is
idempotent at `current` (so the PSP webhook can re-fire safely). It is
rejected on `cancelled` rows — a payment that lands on a cancelled
subscription is an invoice-domain concern, not a dunning concern, and must
be resolved by issuing a fresh subscription.

### 4. Administrative override (CourtesyGrant)

Master may grant a temporary reprieve via
`CourtesyGrant.kind=free_subscription_period` ([ADR 0093](./0093-courtesy-grant-on-tenant-creation.md))
targeting the subscription. The dunning domain consumes the grant as the
value type [`dunning.Override`](../../internal/billing/dunning/port.go)
(`{Until time.Time; Reason string}`) sourced through the
`CourtesyOverride` port.

`DunningState.ApplyOverride(until, reason, now)` semantics:

- `reason` MUST be ≥10 chars (mirrors the DB CHECK
  `subscription_dunning_states_override_consistency`).
- `until` MUST be strictly after `now` (no retroactive reprieves).
- Rejected on `cancelled` rows.
- If the row was above `current`, it resets to `current` and clears
  `last_invoice_id`. Rationale: the grant pays for the current period; if
  the override expires unpaid, the cron will re-escalate based on the
  **next** invoice's due date.

`ClearOverride()` revokes the reprieve without changing state — escalation
resumes on the next cron tick.

### 5. Persistence and audit

Migration
[`0102_phase4_marketing_billing_dunning.up.sql`](../../migrations/0102_phase4_marketing_billing_dunning.up.sql)
provisions:

- `subscription_dunning_states` (one row per subscription, RLS by
  `tenant_id`).
- The state CHECK constraint enumerated in §1.
- `subscription_dunning_states_override_consistency` enforcing
  `(override_until IS NULL) ↔ (override_reason IS NULL)` and
  `char_length(override_reason) >= 10` when set.
- Master-ops audit via `master_ops_audit_trigger` so every write records
  `actorID`.

Reads use the `app_runtime` role under RLS; writes require `master_ops`
(the dunning cron and the PSP-webhook handler) — least privilege per
[ADR 0071](./0071-postgres-roles.md).

## Alternatives considered

- **Option (b) — alerta apenas.** Rejected. COGS on channels and LLM
  spend grows linearly with volume; without a technical lever a single
  high-volume inadimplente sinks the plan margin.
- **Bloqueio binário no D+1.** Rejected. Hostile UX and unnecessary
  churn; six days of banner is a fair window to regularise.
- **Pre-payment by consumable credits (token wallet).** Out of scope — that
  changes the commercial model. May be revisited in D4 ([SIN-62207](/SIN/issues/SIN-62207)).
- **Status field on `subscription` instead of a separate aggregate.**
  Rejected. The dunning lifecycle has its own invariants (terminal
  `cancelled`, override semantics, severity ordering) that don't compose
  cleanly with the financial status enum, and master-ops audit is cleaner
  with a dedicated table. The aggregate-root model also lets us add new
  states (e.g. an explicit `paused_by_master`) without churning the
  subscription table.
- **Subscription-status flag (`active|past_due|suspended|cancelled`) as
  drafted in the original `0086` branch.** Rejected for the same reason:
  the draft conflated billing status with dunning state and would have
  forced every dunning concern through subscription mutations. The shipped
  enum is the dunning lifecycle in isolation.

## Consequences

Positives:

- Variable COGS has a 7-day cap per inadimplente — the outbound block zeros
  the dominant channel/LLM bleed long before it becomes meaningful debt.
- The end customer keeps receiving inbound and consulting history even at
  D+30; degradation is write-side only.
- Policy is configurable per plan — commercial team is not bound to a
  single SLA.
- Master has an auditable lever (`CourtesyGrant.kind=free_subscription_period`)
  for legitimate delays, with a TTL and a mandatory reason.
- Domain is pure (no `database/sql`, no PSP SDK imports) — exercises the
  hexagonal port-and-adapter rule. Persistence and PSP integration live
  behind `DunningRepository` and `CourtesyOverride`.

Negatives / costs to be paid in downstream issues:

- The cron worker that calls `Escalate` is **not yet shipped** —
  scheduled for `SIN-62965` (C14). Until that lands, dunning is dormant.
- Outbound gateway hook — every send path must look up
  `subscription_dunning_states.state` for the tenant and refuse on
  `suspended_outbound`/`suspended_full`/`cancelled`. Owners: channel-adapter
  PRs in Fase 4 (`internal/messaging/*`).
- HTMX banners — server-rendered banner per state (`warn`/`suspended_*`),
  daily-email template per band. Owner: frontend issues in Fase 4.
- Auto-renegotiation UX (button that re-issues 2ª-via PIX) to reduce
  master-ops chargeback load — out of scope here, follow-up issue.

## Verification

The domain tests in
[`internal/billing/dunning/*_test.go`](../../internal/billing/dunning)
cover:

- `state_test.go` — severity ordering, terminal detection, unknown-state
  defence.
- `policy_test.go` — validation of strictly-increasing positive
  thresholds, `StateForDaysPastDue` boundary cases (D-1, D+0, D+1, D+6,
  D+7, D+29, D+30, D+89, D+90).
- `dunning_test.go` — `Escalate` monotonicity, `Escalate` no-op on
  `cancelled`/active-override, `MarkPaid` idempotency, `ApplyOverride`
  reason ≥10 chars + reset to `current` from elevated states,
  `ClearOverride` no-op chain.
- `port_test.go` — repository contract (round-trip + `ErrNotFound` +
  `ErrZeroSubscription`).

Open verification owed by downstream issues:

1. **Cron integration.** `SIN-62965` must drive `Escalate` on every
   non-`cancelled` row with a past-due invoice on a configurable cadence
   (default 15 min) and respect the override.
2. **Outbound gateway gate.** Adapter test: a tenant in
   `suspended_outbound` produces `ErrSubscriptionOutboundBlocked` (or the
   gateway-level equivalent) without dispatching to the channel.
3. **PSP webhook → `MarkPaid`.** End-to-end: paying an invoice on a
   `suspended_full` row resets the dunning state to `current` and
   re-enables outbound in the same transaction.
4. **Override audit.** `CourtesyGrant` revocation flows through
   `ClearOverride` and the next cron tick re-escalates correctly.

## Out of scope (separate decisions)

- **D2** — PSP choice for PIX: [SIN-62205](/SIN/issues/SIN-62205).
- **D4** — pricing of add-on token packs: [SIN-62207](/SIN/issues/SIN-62207).
- Retention window after `cancelled`: dedicated LGPD ADR (Fase 4
  retention parent).
- Multi-attempt automatic PSP retry policy: Fase 4 implementation issue,
  depends on D2.
- HTMX banner + email-per-band templates: Fase 4 frontend issues.

## Rollback

Policy is per-plan and JSON-shaped at the plan level (see [ADR 0097](./0097-subscription-and-invoice-model.md)).
To suspend the bloqueio temporarily (incident, PSP outage, end-of-month
commercial freeze) update every plan with very large thresholds
(e.g. `{1, 9999, 9999, 9999}`) — effect is immediate, no deploy required.
The same lever can be exposed as a global master toggle for emergencies.

For a full revert of the feature: drop the cron job (no escalation),
disable the outbound-gateway gate (no functional block), and leave the
table populated — the rows are inert without the cron driving them and
can be re-enabled later without data loss.
