# ADR 0093 — Courtesy grant on tenant creation

- Status: accepted
- Date: 2026-05-15
- Deciders: CTO, Coder
- Tickets:
  [SIN-62730](/SIN/issues/SIN-62730) (this ADR — Fase 1 PR11),
  [SIN-62727](/SIN/issues/SIN-62727) (PR5 wallet domain — provides `Service.Grant`),
  [SIN-62725](/SIN/issues/SIN-62725) (PR2 migration — `token_wallet`, `courtesy_grant`, `token_ledger`)
- Related: [ADR 0071](./0071-postgres-roles.md) (Postgres roles), [ADR 0088](./0088-wallet-atomic-reserve.md) (F30/F37),
  `migrations/0089_wallet_basic.up.sql`,
  `internal/wallet/courtesy.go`,
  `internal/wallet/usecase/issue_courtesy_grant.go`,
  `internal/adapter/db/postgres/wallet/courtesy.go`,
  `internal/tenancy/courtesy.go`,
  `cmd/server/courtesygrant_wire.go`

## Context

The MVP token economy (Fase 1, [SIN-62193](/SIN/issues/SIN-62193)) ships
WhatsApp messaging gated by a per-tenant token wallet. Without a
positive starting balance, a fresh tenant has no available capacity:
`Reserve` returns `ErrInsufficientFunds`, the message is silently
dropped (or, worse, surfaces an error to a user who just signed up).

The PR2 migration already shipped the three tables involved
([SIN-62725](/SIN/issues/SIN-62725)):

- `token_wallet` — per-tenant aggregate (`UNIQUE (tenant_id)`).
- `courtesy_grant` — the audit row recording the onboarding credit
  (`UNIQUE (tenant_id)`, master_ops-only writes).
- `token_ledger` — append-only journal carrying
  `UNIQUE (wallet_id, idempotency_key)` partial index for the
  wallet-aware lane.

PR5 ([SIN-62727](/SIN/issues/SIN-62727)) shipped `wallet.Service.Grant`,
which credits an *existing* wallet. The missing piece is the
bootstrap path: emit all three rows together at tenant creation.

This ADR locks the design for that bootstrap and the consumer-side
port that the future master tenant-create flow will call.

## Decision

1. **Run the bootstrap in a single `WithMasterOps` transaction.**

   `courtesy_grant` is INSERTable only by `app_master_ops` (migration
   0089 §courtesy_grant). The wallet bootstrap could in principle run
   under `WithTenant`, but splitting the flow across two transactions
   leaves a window where a crashed onboarding leaks an orphan tenant
   without a wallet — and the master_ops audit chain would miss the
   wallet row.

   The full four statements (claim grant slot → insert wallet → insert
   ledger row → materialize balance) therefore live in **one**
   `postgres.WithMasterOps` transaction, with `app.master_ops_actor_user_id`
   set to a configured onboarding user.

2. **Use `INSERT … ON CONFLICT (tenant_id) DO NOTHING RETURNING id`
   on `courtesy_grant` as the idempotency choke point.**

   Concurrent calls for the same `tenant_id` serialise on the
   partial-unique conflict; the loser's `RETURNING` is empty, the
   adapter swallows `pgx.ErrNoRows`, re-reads the surviving grant +
   wallet, and returns `Issued{Granted: false}`. This avoids the
   SAVEPOINT dance that would otherwise be required to recover from
   a 23505 raised by a plain `INSERT`.

3. **Defense in depth via `token_ledger.UNIQUE (wallet_id, idempotency_key)`.**

   Every ledger row inserted by the bootstrap carries
   `idempotency_key = "courtesy:" || tenant_id`. The partial-unique
   index on `token_ledger` then physically prevents a second grant
   row from landing under that key even if the `courtesy_grant`
   row were ever deleted out from under us. Both constraints together
   are the F30/F37-style "two locks" pattern (ADR 0088 §"defense in
   depth").

4. **Consumer-side port owned by `internal/tenancy`.**

   The future master tenant-create flow lives next to the `Tenant`
   aggregate. It depends on `tenancy.CourtesyGrantIssuer` (an
   interface declared in `internal/tenancy/courtesy.go`); the
   wire-up at `cmd/server` adapts
   `wallet/usecase.IssueCourtesyGrantService` to that interface via
   `courtesyIssuerAdapter`. This keeps `internal/tenancy` free of
   any `internal/wallet` or `internal/adapter/...` imports.

5. **Three-knob config with fail-closed boot.**

   - `COURTESY_GRANT_TOKENS` (default `10000`) — the credit amount.
   - `COURTESY_GRANT_DISABLED` (default `false`) — soft kill switch
     for dev/CI images that want to skip the bootstrap entirely.
     When set, the wire-up returns a nil service and the consumer
     treats every call as a no-op.
   - `COURTESY_GRANT_ACTOR_ID` (uuid) — the master_ops user stamped
     on every audit row. **Required when the flow is enabled.** A
     missing actor returns `ErrCourtesyActorRequired` so cmd/server
     boots non-zero rather than reaching a runtime null actor.

6. **The wire-up is hot but not yet mounted.**

   The master tenant-create endpoint (`action master.tenant.create`)
   is a separate future ticket. PR11 ships the service constructor,
   the env knobs, and the consumer port; the consumer is a follow-up
   PR. This keeps PR11 reviewable in isolation and avoids a
   half-wired path reaching production traffic.

## Alternatives considered

### A. Single `app_runtime` transaction with `WithTenant`

`token_wallet` is INSERTable by `app_runtime`. We could call this
under `WithTenant(newTenantID)` from the future tenant-create flow.
**Rejected** because `courtesy_grant` is master_ops-only — splitting
the flow across two transactions (runtime for wallet, master_ops for
grant) leaks the half-applied state on crash, and the master_ops
audit chain would miss the wallet row.

### B. Two transactions, with a reconciler sweep for orphans

Run the wallet bootstrap inline (runtime) and emit the
`courtesy_grant` + ledger row in a follow-up master_ops tx. A
periodic reconciler would re-grant any wallet without a courtesy
record. **Rejected** because the reconciler is a substantial extra
moving part for a flow that is naturally atomic at the database
layer (`courtesy_grant.tenant_id UNIQUE` already serialises the
race). The single-tx design fits within ADR 0088's "boring tech"
budget.

### C. Inline the wiring into a stubbed `master.tenant.create` handler

Land the consumer of `tenancy.CourtesyGrantIssuer` in the same PR.
**Rejected** for review-size reasons (the handler is its own ~400
LoC ticket once the IAM-action plumbing is exercised end to end).
This ADR records the contract so the follow-up PR has nothing to
re-litigate.

## Consequences

**Positive**

- Bootstrap is atomic from the operator's perspective: either every
  row lands or none of them do.
- Retried tenant creates are safe — the grant is no-op on duplicate.
- 50 concurrent creates for the same tenant collapse to a single
  wallet + grant + ledger row (validated by
  `TestCourtesyStore_FiftyConcurrentIssues`).
- The `Disabled` knob lets dev/CI images skip the bootstrap without
  conditional code in the consumer.

**Negative / open follow-ups**

- The acceptance criterion "Integration test ponta-a-ponta (tenant
  create → first message send → débito)" cannot be exercised yet —
  neither the master tenant-create endpoint nor the first WhatsApp
  send path exist. The Postgres adapter integration tests
  (`TestCourtesyStore_*`) cover the bootstrap atomically; the
  end-to-end test is deferred to the PR that lands the consumer
  (master tenant-create flow).
- The "Lint custom: `internal/tenancy` não importa `internal/wallet`
  adapter" requirement is satisfied structurally (the tenancy
  package only imports the consumer port it owns), but is not yet
  enforced by an analyzer. A future PR can extend
  `tools/lint/forbidimport` with a per-package allowlist if the
  community wants the rule machine-checked.

## Rollback

The whole flow is feature-flagged. Setting
`COURTESY_GRANT_DISABLED=1` reverts the consumer to a no-op without
a code change. The migrations themselves carry forward — the
courtesy tables were already shipped in
[SIN-62725](/SIN/issues/SIN-62725) (`migrations/0089_wallet_basic.up.sql`).
