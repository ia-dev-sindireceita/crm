# ADR 0042 — AI policy cascade: channel > team > tenant, all-or-nothing override

- Status: Accepted
- Date: 2026-05-16
- Deciders: CTO
- Drives: [SIN-62901](/SIN/issues/SIN-62901) (this ADR), [SIN-62196](/SIN/issues/SIN-62196) (Fase 3 parent)

## Context

Fase 3 ([SIN-62196](/SIN/issues/SIN-62196)) ships per-tenant
configuration for AI assist: which model to call, whether to
anonymise prompts, what the per-request token cap is, which scopes
(summarise / draft / extract) are enabled. Different parts of a
tenant want different settings:

- A particular *channel* (one WhatsApp number used for an enterprise
  customer) needs the largest model and may legitimately opt out of
  anonymisation because that customer has signed a separate DPA.
- A *team* (one operator squad) needs a smaller per-request cap to
  control spend.
- The *tenant* itself sets the default for everything else.

This is the classic configuration-hierarchy problem. The naive
options:

- **One row per tenant.** Cannot express channel- or team-specific
  variation; tenants with mixed needs cannot use the feature.
- **One row per (tenant, channel, team).** Combinatorial explosion;
  operators cannot reason about which row applied to which call.
- **Hierarchical with field-level merge.** Each level fills in the
  fields the lower level did not specify. Feels flexible. In
  practice, it is a debugging nightmare ("why did this call use
  channel A's model but team B's anonymise flag?") and a security
  surprise ("the team-level row disables anonymisation but the
  channel-level row enables it; which wins?").

The lens **least privilege** says each scope should carry exactly
the policy that scope wants applied — not a partial overlay that
inherits unrelated fields from another scope. The operator should
be able to read one row and know what runs.

The lens **observability before optimisation** says the resolver
must be able to produce a deterministic "for this call, this policy
applied, because of this row" answer at audit time. Field-level
merge makes this answer a paragraph; all-or-nothing makes it a
single row id.

## Decision

**Effective policy is resolved by a strict cascade — channel >
team > tenant > default — implemented in a pure resolver
`internal/aipolicy/resolver.go`. The cascade returns the first
matching row in full. There is no field-level merge: an override
at any level replaces the entire policy for that call.**

### D1 — Schema

The `ai_policy` table:

```sql
CREATE TABLE ai_policy (
    id                   UUID PRIMARY KEY,
    tenant_id            UUID NOT NULL REFERENCES tenant(id),
    scope_type           TEXT NOT NULL CHECK (scope_type IN ('channel', 'team', 'tenant')),
    scope_id             UUID NOT NULL,      -- channel.id / team.id / tenant.id
    enabled              BOOLEAN NOT NULL,
    anonymize            BOOLEAN NOT NULL,
    model_id             TEXT NOT NULL,
    max_tokens_per_call  INT NOT NULL,
    enabled_scopes       TEXT[] NOT NULL,    -- summarise / draft / extract
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, scope_type, scope_id)
);
```

Notes:

- `scope_type = 'tenant'` implies `scope_id = tenant_id`. Enforced
  by a partial CHECK and by the application layer; a constraint
  trigger would be over-engineered for the value.
- RLS policies on this table (per [ADR 0072](./0072-rls-policies.md))
  restrict each tenant to its own rows. The resolver always runs
  inside a tenant-scoped Postgres role.

### D2 — Resolver algorithm

`internal/aipolicy/resolver.go`:

```go
type ResolveInput struct {
    TenantID  uuid.UUID
    ChannelID *uuid.UUID // optional — nil when the call is not channel-scoped
    TeamID    *uuid.UUID // optional — nil when the call is not team-scoped
}

func (r *Resolver) Resolve(ctx context.Context, in ResolveInput) (Policy, ResolveSource, error)
```

`ResolveSource` is a small typed enum that the audit pipeline
records: `SourceChannel`, `SourceTeam`, `SourceTenant`,
`SourceDefault`.

Algorithm:

1. If `in.ChannelID != nil`: look up
   `(tenant_id = in.TenantID, scope_type = 'channel', scope_id = *in.ChannelID)`.
   On hit, return that row + `SourceChannel`.
2. If `in.TeamID != nil`: look up
   `(tenant_id = in.TenantID, scope_type = 'team', scope_id = *in.TeamID)`.
   On hit, return that row + `SourceTeam`.
3. Look up `(tenant_id = in.TenantID, scope_type = 'tenant', scope_id = in.TenantID)`.
   On hit, return that row + `SourceTenant`.
4. Return `DefaultPolicy()` + `SourceDefault`.

The default policy (`DefaultPolicy()`) is a hard-coded constant in
the package:

```go
func DefaultPolicy() Policy {
    return Policy{
        Enabled:           false,  // AI assist is off until a tenant explicitly enables it
        Anonymize:         true,   // default-on per board decision #8 on SIN-62203
        ModelID:           "",     // empty → caller must reject; the default policy never authorises a call
        MaxTokensPerCall:  0,
        EnabledScopes:     nil,
    }
}
```

`Enabled: false` and `ModelID: ""` make the default policy a
**deny** outcome. The use-case in `internal/aiassist/usecase.go`
must reject the call when `policy.Enabled == false`. A tenant that
has never been configured cannot call the LLM.

The resolver is **pure**: given the same Postgres state and the
same `ResolveInput`, it returns the same `Policy` and the same
`ResolveSource`. It has no side effects, no caches, no clocks.
This makes it trivially unit-testable with a fake repository.

### D3 — All-or-nothing override (no field-level merge)

The deliberate non-feature: when a channel-level policy exists, it
is returned in full. The tenant-level policy is **not** consulted
to fill in fields the channel level did not set. The same applies
to team vs tenant.

This is the **least privilege** lens applied to configuration:
each row carries only its own scope's policy. The operator sees
one row and knows what runs.

Consequences:

- If a tenant adds a channel-level override, they must restate
  every field they want at that channel — including the ones
  identical to the tenant default. The admin UI surfaces this
  explicitly: "this override replaces the tenant default; here is
  the diff."
- A typo at the channel level cannot silently inherit the
  tenant's safer default. If `anonymize` is omitted in the form,
  the row's `anonymize` is `false` — visible in the diff, not
  silently filled in.
- Bulk edits across many channels are slightly more verbose. The
  admin UI mitigates with a "apply tenant default to N channels"
  action; the API requires the full row.

The trade-off is intentional: surprise is a security risk; verbose
configuration is a UI problem.

### D4 — Resolver is pure; the repository is the adapter

`internal/aipolicy/resolver.go` defines:

```go
type Repository interface {
    LookupByScope(ctx context.Context, tenantID uuid.UUID, scopeType ScopeType, scopeID uuid.UUID) (Policy, bool, error)
}

type Resolver struct {
    Repo Repository
}
```

The repository is the only thing that touches Postgres. The
adapter lives at `internal/adapter/aipolicy/pgrepo.go` and is the
only file that imports `database/sql` (or the project's `pgxpool`
wrapper). Lint rule `no-sql-in-domain` blocks SQL imports from
`internal/aipolicy/`.

Three lookups maximum per resolve call (one per scope, short-
circuit on hit). Each lookup is by the `(tenant_id, scope_type,
scope_id)` UNIQUE index — index-only scans, sub-millisecond
typical.

### D5 — Caching deferred

Fase 3 ships the resolver **without** a cache. Reasons:

- Three index-only point lookups per call cost less than the LLM
  call itself by four orders of magnitude. Premature optimisation.
- A cache adds invalidation complexity (when the admin changes a
  policy, downstream calls must see it within seconds). Today,
  each call reads the live row.
- The lens **observability before optimisation** says: ship without
  the cache, measure, then decide. The latency metric
  `aipolicy.resolve_ms` will tell us if a cache is needed.

A future ADR may add a tenant-keyed local cache with TTL ≤ 5s and
an admin-triggered bust path. Not Fase 3.

### D6 — Audit

Every `Resolve` call emits one structured audit record:

```json
{
  "event": "ai.policy.resolved",
  "ts": "2026-05-16T19:24:11.456Z",
  "tenant_id": "01HXYZ...",
  "channel_id": "01HXYZ...",
  "team_id": null,
  "policy_id": "01HXYZ...",
  "source": "channel",
  "anonymize": false,
  "model_id": "openrouter/gpt-4-turbo",
  "max_tokens": 4000
}
```

The audit record is emitted **before** the LLM call. If the call
later fails or is rolled back, the audit row stays — it records
what *was authorised*, not what ran. This separation is the
forensic foundation: "tenant X used model Y on date Z" is
answerable from the audit log alone.

When `source == "default"`, the audit record uses
`policy_id: null` and `source: "default"` so dashboards can count
"calls authorised by hard-coded default policy" (should be zero
for any tenant with assist enabled).

### D7 — Hexagonal boundary

`internal/aipolicy/` contains:

- `policy.go` — `Policy` value type, `ScopeType` enum,
  `DefaultPolicy()`.
- `resolver.go` — `Resolver`, `ResolveInput`, `ResolveSource`,
  the cascade algorithm. No SQL, no HTTP, no SDK.
- `repository.go` — the `Repository` port.

`internal/adapter/aipolicy/pgrepo.go` is the Postgres adapter.

The use-case (`internal/aiassist/usecase.go`) calls
`resolver.Resolve(...)` once per `Generate` call, between the
authentication/authorisation check and the rate limiter. The
resolved `Policy` is threaded through the rest of the use-case;
the LLM port, the anonymizer, and the wallet all read from it.

## Consequences

Positive:

- **Least privilege**: each policy row scopes a single
  configuration intent. No silent merge.
- **Hexagonal**: pure resolver, swappable repository, audit
  emitter is the only side effect (and it lives in the use-case,
  not the resolver).
- Deterministic, replayable behaviour: given the policy table
  state at time `t`, the resolver produces the same `Policy` for
  any call.
- Operators can reason about which row applied: the audit log
  records `policy_id` and `source` per call.
- Default policy is a deny: a tenant that has not configured AI
  assist cannot accidentally make LLM calls because of a
  resolver bug or a missing row.

Negative / costs:

- Admin UI is wordier when a tenant has many channels. Each
  override is a complete policy, not a delta. Mitigated by the
  admin UI surface ("show diff vs tenant default", "bulk apply
  tenant default to N channels").
- Field-level merge enthusiasts will complain that the design is
  inflexible. The reply: the inflexibility is the feature.
- Three lookups per call. Today: free (sub-ms). Tomorrow: a
  measured cache if `aipolicy.resolve_ms` exceeds the threshold.

Risk residual:

- Operator edits a tenant-default policy expecting it to apply
  to a channel that has an override. The override wins; the
  tenant default change has no effect for that channel. UI must
  surface the override list when a tenant default is edited.
  Mitigation: the admin UI's tenant-default form shows the count
  of active channel and team overrides and a link to inspect
  them.
- A new channel created after a tenant-default policy was set
  must explicitly inherit the default (the resolver does that
  automatically because there is no channel override row). If
  an admin then adds a partial channel override expecting
  inheritance to fill the rest, they will see an unexpected
  result. The admin UI's create-override form pre-fills the form
  with the tenant default and requires the admin to acknowledge
  the row as a full replacement.

## Alternatives considered

### Option A — Field-level merge (channel overlays tenant)

For each field, take the deepest non-null value across the
cascade.

Rejected because:

- "Why did this call use these settings?" becomes a multi-field
  diff explanation instead of a row id. Forensics get harder.
- Adds null-vs-not-null logic to every field; the schema becomes
  full of `*bool`, `*string`, `*int`. Idiomatic-Go and the
  Postgres schema get noisier.
- Allows a partial misconfiguration at a deep scope to silently
  inherit unrelated fields from a shallower scope. **Least
  privilege** violation: a channel that wanted to override the
  model now also inherits the team's tighter token cap, which the
  channel admin did not request and may not realise.
- Makes "default policy" ambiguous: is the default the bottom of
  the cascade, or the per-field fallback for the cascade? Two
  competing meanings.

### Option B — Single config blob per tenant, JSON path overrides

Store a single JSON document per tenant; allow path-based
overrides for channel and team.

Rejected because:

- JSON-path overrides are field-level merge with extra syntax.
  Same forensic problem.
- Validation is harder: each path's expected type lives in
  application code, not the schema. Migrations become brittle.
- The boring-tech budget favours typed columns + UNIQUE index
  over JSONB hierarchies for what is fundamentally a small,
  flat configuration object.

### Option C — Cascade with channel > tenant > team (different order)

A tenant override beats a team override.

Rejected because:

- Channels represent concrete customers / contracts (one
  WhatsApp number = one enterprise account). Their policies need
  to win over both team and tenant defaults — the customer's DPA
  is more specific than the team's operational preference.
- Teams represent operator squads. Their preferences (spend caps,
  enabled scopes) are organisationally above the tenant default
  but below the customer-contract level.
- Reversing the order would let a tenant-wide knob silently
  weaken a team's stricter policy, which is the opposite of
  what the operator squad wants.

The chosen order — **channel > team > tenant** — is the only
one that produces "more specific wins" at every level.

### Option D — Resolver lives in the use-case

Inline the cascade lookup directly in
`internal/aiassist/usecase.go`.

Rejected because:

- The resolver has its own bounded responsibility and its own
  audit event. It is independently testable.
- The same resolver is used by future admin UI flows ("show me
  the policy that *would* apply to channel X right now") and by
  the operational dashboards. A shared callable beats inlining.

## Lenses cited

- **Least privilege.** Each policy row holds only its scope's
  policy. No silent inheritance.
- **Hexagonal / ports & adapters.** Pure resolver, Postgres
  adapter behind a port, audit emission in the use-case.
- **Reversibility & blast radius.** A bad policy change is
  reverted by updating one row. A bad admin UI is rolled back
  without touching the resolver.
- **Observability before optimisation.** No cache yet; latency
  metric ships day one and decides whether one is needed.
- **Boring technology budget.** A single table with a UNIQUE
  index, three point lookups, hard-coded default.

## Out of scope

- A cache layer in front of the repository. Deferred until
  latency metrics warrant it.
- Time-based policy (different policies for business hours vs
  off-hours). Out of scope for Fase 3; if needed, a future ADR
  will add a scheduling layer above this resolver.
- Master-level policy (a CRM operator imposes a policy on all
  tenants in their company). Out of scope; master-level controls
  ship as part of the master surface in a later phase.
- A/B testing of policies (random assignment per call). Not
  needed in Fase 3.
