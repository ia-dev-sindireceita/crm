# ADR 0109 — Per-channel access: surface-role gate + per-resource membership gate (two-layer authz)

- Status: Accepted
- Date: 2026-07-01
- Deciders: CTO, Coder, SecurityEngineer (review gate)
- Drives: [SIN-66392](/SIN/issues/SIN-66392) (P3 — this ADR governs the enforcement it lands with)
- Parent: [SIN-66378](/SIN/issues/SIN-66378) (multi-channel per tenant), [SIN-66375](/SIN/issues/SIN-66375)
- Extends: [ADR 0090](0090-rbac-matrix.md) (RBAC matrix + `iam.Authorizer`)
- Lenses: **OWASP A01 (broken access control)**, **Defense in depth**, **Hexagonal / DDD-lite**, **Reversibility**

## Context

The multi-channel-per-tenant work ([SIN-66378](/SIN/issues/SIN-66378))
introduces the codebase's **first per-resource authorization gate**. Until
now every access decision has been a pure function of `(role, tenant)`:
the RBAC matrix in [ADR 0090](0090-rbac-matrix.md)
(`internal/iam/authorizer.go`) maps an `iam.Action` to the set of roles
allowed to perform it, deterministically and **without touching the
database**. `iam.Authorizer.Can()` already receives a `Resource{ID}` in
its signature, but the RBAC implementation deliberately **ignores
`r.ID`** — it gates the *surface* (may this role reach `/inbox` /
`/settings/channels` at all?), not any individual row.

Per-channel access is different in kind. Two atendentes with the same
role must be able to see *different* sets of conversations, because a
gerente has restricted a channel to a subset of the team
(`tenant_channels.restricted` + the `channel_access` membership table,
migration 0128, [SIN-66389](/SIN/issues/SIN-66389)). That decision
depends on **which channel row** and **which user**, and it must read
per-tenant membership data. It cannot be a pure matrix lookup.

The temptation is to fold this into `iam.RBACAuthorizer` by finally
honouring `Resource{ID}`. We reject that: it would make the matrix
impure (DB-dependent), non-deterministic, and much harder to reason
about and test, and it would entangle two decisions that have different
lifecycles, different data sources, and different failure modes.

## Decision

### D1 — Two independent layers, enforced in series (defense in depth)

Every request to a channel-scoped resource passes **two** gates:

1. **Surface-role gate (ADR 0090, unchanged).** `iam.RBACAuthorizer`
   answers "may this *role* touch this surface at all?" from the pure,
   DB-free matrix. `/settings/channels*` requires
   `ActionTenantChannelsManage` (`RoleTenantGerente`); the inbox
   surface requires the inbox action. This is the coarse gate and stays
   exactly as it is.

2. **Per-resource membership gate (new, this ADR).** A separate
   `channels.AccessService` answers "may this *user* act on *this
   channel instance*?" It reads `tenant_channels.restricted` and the
   `channel_access` grants under tenant RLS. It lives in the
   `internal/channels` domain package, **not** in `internal/iam`.

Neither layer is sufficient alone. The role gate cannot express
per-row membership; the membership gate cannot express "this role has
no business on this surface". Enforced together they are
defense-in-depth: a bug that opens one does not open the other.

### D2 — The per-resource rule (the effective decision)

`channels.Decide(isGerente, restricted, hasGrant) bool` is the pure core:

- **Gerente override** — a gerente always may act on any channel in
  their tenant (a manager owns every channel). The grant table is not
  even consulted.
- **Open channel** (`restricted == false`) — visible to every atendente
  of the tenant. This is the zero-regression posture: the 0128 backfill
  materialised existing channels as open and granted all current users,
  so nobody loses inbox access on deploy.
- **Restricted channel** (`restricted == true`) — limited to the users
  holding an explicit `channel_access` grant (plus the gerente
  override).

The `restricted` flag is the **policy input**, authored by the gerente
in the maintenance UI. Toggling it never rewrites the grant roster, so a
channel round-trips open→restricted→open without re-authoring who has
access. `Decide` is a pure function (exhaustively table-tested); the
DB-backed composition (`AccessService.CanAccessChannel` /
`AccessibleChannelIDs`) resolves the channel + grant and applies it.

### D3 — Signature: tenant threaded explicitly; role passed as a bool

- Every port method is **tenant-scoped** and runs under
  `postgres.WithTenant` so RLS restricts the rows. The
  [SIN-66389](/SIN/issues/SIN-66389) ticket sketched
  `CanAccessChannel(ctx, userID, channelID)` without a tenant; we thread
  `tenantID` explicitly, matching every other repository port and making
  a tenant-less (RLS-leaking) read impossible.
- `AccessService` takes `isGerente bool` rather than importing
  `internal/iam` to resolve the role. The role lives in the surface
  layer; passing the resolved boolean down keeps the `channels` domain
  package free of an iam dependency (accept-broad, no import cycle). The
  surface layer (P4 inbox read path) resolves the role from context and
  passes the bool.

### D4 — Fail-closed on unknown channel, even for gerente

A non-existent or RLS-hidden channel id yields `(false, nil)` for every
caller, including a gerente. The gerente override grants access to the
tenant's *real* channels, not to a phantom id an adversary might probe;
collapsing unknown-channel to a plain deny also means a caller cannot
distinguish "exists but you lack access" from "does not exist".

## Alternatives considered

- **Honour `Resource{ID}` inside `iam.RBACAuthorizer`.** Rejected — makes
  the matrix impure and DB-dependent, entangles two decisions with
  different data sources and lifecycles, and defeats the value of a
  deterministic, unit-testable matrix (ADR 0090).
- **Enforce membership only in the UI (hide unpermitted channels).**
  Rejected — that is not access control (OWASP A01). Enforcement lives
  server-side in `AccessService`; the UI merely reflects it. P4 filters
  the *live* `ListConversationSummaries` read path, not just the render.
- **Store the effective allow-list per user.** Rejected — denormalised,
  hard to keep consistent on channel/user/grant changes. The membership
  table + `restricted` flag is the normalised source of truth; the
  allow-list is derived on read (`AccessibleChannelIDs`).

## Consequences

- P4 ([SIN-66393](/SIN/issues/SIN-66393)) consumes
  `AccessService.AccessibleChannelIDs` to filter the inbox read path and
  `CanAccessChannel` to guard per-conversation actions; the maintenance
  UI (this PR) consumes `Repository.SetRestricted` + the roster primitive
  to author the policy.
- The two gates are reviewed and tested separately: the matrix by the
  ADR-0090 contract tests, the membership gate by
  `channels.Decide` / `AccessService` unit tests and the postgres adapter
  integration tests.
- **SecurityEngineer review gate:** this is the tenant's first
  per-resource authz. Confirmed items: gerente-only mutate (surface
  gate), atendente cannot self-grant (writes are gerente-gated at the
  route), fail-closed on unknown channel (D4), grants unaffected by the
  restricted toggle (D2), tenant isolation via RLS on every path (D3).

## Rollback

The feature is additive and reversible. New channels default **open**
and the backfill grants everyone, so with no channel flipped to
restricted the effective behaviour is identical to pre-0128 (every
atendente sees every channel). Reverting is: stop consuming
`AccessService` on the read path (P4) and hide the restricted toggle;
the `restricted` column and `channel_access` rows can remain dormant. No
destructive migration is required.

## Out of scope

- The inbox read-path filtering + channel-scope chip (P4,
  [SIN-66393](/SIN/issues/SIN-66393)).
- An audit-log line on every access change — flagged by SecurityEngineer
  in the UX spec (§4); tracked as a follow-up, not gating this ADR.
