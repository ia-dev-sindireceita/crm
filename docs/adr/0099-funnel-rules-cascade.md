# ADR 0099 — Funnel rules cascade resolver: channel > team > tenant, per-trigger first-match-wins

- Status: Proposed
- Date: 2026-05-17
- Deciders: CTO
- Drives: [SIN-62953](/SIN/issues/SIN-62953) (this ADR), [SIN-62197](/SIN/issues/SIN-62197) (Fase 4 parent)
- Builds on: [ADR 0042](./0042-policy-cascade.md) (AI policy cascade — channel > team > tenant, all-or-nothing)
- Related implementation precedent: [SIN-62906](/SIN/issues/SIN-62906) (HTMX config UI + cascade preview, commit `4a5ee2d`)

> **Numbering note.** The originating Paperclip issue ([SIN-62953](/SIN/issues/SIN-62953))
> proposed the file be saved as `0088-funnel-rules-cascade.md`. ADR 0088 is
> already occupied by [`0088-wallet-basic.md`](./0088-wallet-basic.md); per the
> [README](./README.md) convention ("numbers are permanent — supersedes /
> amendments live inside the affected ADR, never via renumbering"), this ADR
> takes the next free index, **0099**.

## Context

Fase 4 ([SIN-62197](/SIN/issues/SIN-62197)) ships **automatic funnel
rules**: rules that move a conversation between funnel stages, or
tag it, in response to a triggering event (inbound message keyword,
campaign click, status change). Acceptance criterion #3 of the
parent issue requires scoped behaviour:

> Regra "msg contém 'orçamento' → `qualificando`" no escopo canal
> Webchat: ativa quando msg do canal Webchat; ignora msg do
> WhatsApp do mesmo tenant.

That is, the operator must be able to author a rule that fires only
for a specific **channel**, only for a specific operator **team**, or
for the entire **tenant**, and the runtime must pick the right rules
per call. Different tenants want different things at different
scopes:

- A particular **channel** (one WhatsApp number used for an
  enterprise customer) needs a customer-specific rule — e.g. "messages
  matching `'NF-\d+'` jump straight to `cobranca` for this customer's
  number" — without polluting the rest of the tenant's funnel.
- A **team** (one operator squad) wants its own qualification
  vocabulary — "any contact mentioning 'preço' moves to `qualificando`
  for the sales squad" — that other squads in the same tenant should
  not inherit.
- The **tenant** itself sets the baseline that applies everywhere
  else — campaign-click landing stage, default keyword routing.

This is the same configuration-hierarchy problem [ADR
0042](./0042-policy-cascade.md) faced for `ai_policy`. The precedent
exists and was validated by the cascade-preview UI shipped in
[SIN-62906](/SIN/issues/SIN-62906) (commit `4a5ee2d`), which lets an
operator pick a scope and see exactly which policy row would apply.
Funnel rules **borrow the cascade order** from 0042 but have one
shape difference that we must take seriously.

`ai_policy` is a single decision per call: which model, which token
cap, anonymise yes/no. The cascade returns *one row, in full*, and
the use-case acts on it. Funnel rules are a **set** per event: a
single inbound message can plausibly match more than one rule (a
keyword rule and a campaign-attribution rule), and we want both to
fire when there is no conflict. We only want the "most specific wins"
behaviour when two rules collide — i.e. when two rules across
different scopes both target the same trigger.

The lens **least surprise** says the operator who wrote the
channel-level rule expects it to override the tenant-level rule for
the same trigger, not silently merge with it. The lens
**observability before optimisation** says the resolver must, per
fired rule, record *why* it fired (which scope, which row id) at
audit time. The lens **least privilege** says a scope's rules must
not silently inherit unrelated rules from another scope.

## Decision

**The effective rule set for a triggering event is the union of all
rules whose scope matches the event, evaluated by a pure resolver
`internal/funnel/rules/resolver.go`. When two rules across different
scopes share the same trigger key, the most-specific scope wins —
strict order channel > team > tenant — and the loser is dropped from
the result set. Rules with distinct trigger keys all survive the
cascade. The resolver returns the ordered, deduplicated rule list
plus per-rule provenance so the audit pipeline records `(rule_id,
source_scope)` for every action taken.**

### D1 — Schema: `funnel_rule`

```sql
CREATE TABLE funnel_rule (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID        NOT NULL REFERENCES tenant(id),
    scope_type        TEXT        NOT NULL
        CHECK (scope_type IN ('channel', 'team', 'tenant')),
    scope_id          UUID        NOT NULL,                       -- channel.id / team.id / tenant.id
    trigger_kind      TEXT        NOT NULL
        CHECK (trigger_kind IN ('message_contains', 'campaign_click', 'conversation_idle')),
    trigger_key       TEXT        NOT NULL,                       -- canonicalised match key (lower-cased keyword, campaign id, ...)
    action_kind       TEXT        NOT NULL
        CHECK (action_kind IN ('move_to_stage', 'tag_conversation')),
    action_payload    JSONB       NOT NULL,                       -- {"stage_key":"qualificando"} | {"tag":"vip"}
    enabled           BOOLEAN     NOT NULL DEFAULT TRUE,
    position          INT         NOT NULL DEFAULT 0,             -- tie-break within the same scope, ascending
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, scope_type, scope_id, trigger_kind, trigger_key)
);

CREATE INDEX funnel_rule_scope_idx
    ON funnel_rule (tenant_id, scope_type, scope_id) WHERE enabled;
```

Notes:

- `scope_type = 'tenant'` implies `scope_id = tenant_id` (enforced
  in the application layer, mirroring [ADR 0042](./0042-policy-cascade.md) D1).
- RLS policies on this table (per [ADR
  0072](./0072-rls-policies.md)) restrict each tenant to its own
  rows; the resolver always runs inside a tenant-scoped Postgres
  role.
- The UNIQUE constraint on `(tenant_id, scope_type, scope_id,
  trigger_kind, trigger_key)` makes "same trigger across scopes" the
  exact case the cascade has to resolve.
- `position` only orders rules **within the same scope** that share a
  trigger — used to give operators a deterministic way to break ties
  between two same-scope same-trigger rules (e.g. two `tenant`-scoped
  `message_contains:preço` rows, oldest wins by position). Cross-scope
  conflicts are resolved by the cascade, never by position.

### D2 — Resolver: pure function, three lookups, one merge

`internal/funnel/rules/resolver.go`:

```go
type Trigger struct {
    Kind triggerKind // message_contains / campaign_click / conversation_idle
    Key  string      // already canonicalised by the caller
}

type ResolveInput struct {
    TenantID  uuid.UUID
    ChannelID *uuid.UUID // nil when the event is not channel-bound
    TeamID    *uuid.UUID // nil when the event is not team-bound
    Triggers  []Trigger  // one event may carry several candidate triggers
}

type ResolvedRule struct {
    Rule        Rule        // full domain rule
    SourceScope ScopeType   // channel | team | tenant
}

func (r *Resolver) Resolve(ctx context.Context, in ResolveInput) ([]ResolvedRule, error)
```

Algorithm:

1. For each scope `S` in `[channel, team, tenant]` whose scope id is
   non-nil in `in`, ask the repository for `(tenant_id, S, scope_id,
   enabled=true)` rules whose `trigger_kind|trigger_key` is in
   `in.Triggers`. At most three repository calls per resolve, each
   one a `(tenant_id, scope_type, scope_id) WHERE enabled` index
   scan.
2. Walk scopes in cascade order (channel, then team, then tenant).
   For each rule, key it by `(trigger_kind, trigger_key)`. If a rule
   with that key was already adopted from a more-specific scope,
   **drop the current rule** (first-match-wins per trigger). Otherwise
   adopt it with `SourceScope = S`.
3. Within a single scope, ties on the same `(trigger_kind,
   trigger_key)` are broken by ascending `position`, then by
   ascending `created_at`. This is deterministic by the (tenant_id,
   scope_type, scope_id, trigger_kind, trigger_key) UNIQUE constraint
   — there is at most one row per (scope, trigger) tuple.
4. Return the adopted rules in cascade order (channel rules first,
   then team rules, then tenant rules). Callers iterate in that order
   so audit records reflect the cascade ordering naturally.

The resolver is **pure**: same Postgres state + same `ResolveInput`
→ same `[]ResolvedRule`. No clocks, no caches, no side effects.
Three index-only scans per call; total work is bounded by the number
of candidate triggers `in.Triggers`, which is small (1–3 typical).

### D3 — Cascade example (worked)

Tenant `acme` has rules:

| Scope        | scope target           | trigger_kind       | trigger_key | action               |
|--------------|------------------------|--------------------|-------------|----------------------|
| `tenant`     | acme                   | `message_contains` | `preço`     | move to `qualificando` |
| `tenant`     | acme                   | `campaign_click`   | `camp-X`    | move to `novo`        |
| `team`       | sales squad            | `message_contains` | `preço`     | move to `negociacao`  |
| `channel`    | Webchat embed channel  | `message_contains` | `preço`     | move to `webchat-lead`|
| `channel`    | Webchat embed channel  | `message_contains` | `orçamento` | move to `qualificando`|

Inbound event: a message arriving on the Webchat channel, assigned
to the sales squad, body "Olá, queria saber o **preço** e o
**orçamento** de NF-1234".

The use-case extracts candidate triggers `[message_contains:preço,
message_contains:orçamento]` and calls `Resolve` with `ChannelID =
webchat, TeamID = sales, TenantID = acme, Triggers = [preço,
orçamento]`.

Resolver walk:

1. **Channel scope (Webchat)**: matches both triggers. Adopt
   - `preço → webchat-lead` (source: channel)
   - `orçamento → qualificando` (source: channel)
2. **Team scope (sales)**: rule `preço → negociacao` *would* match,
   but trigger `preço` was already adopted from channel. **Drop.**
3. **Tenant scope (acme)**: rule `preço → qualificando` similarly
   **dropped** (already adopted from channel). The campaign-click
   rule does not match this event's triggers and is irrelevant here.

Final resolved set:

| order | rule_id                | source  | trigger                  | action               |
|-------|------------------------|---------|--------------------------|----------------------|
| 1     | …Webchat-preço         | channel | message_contains:preço   | move to webchat-lead |
| 2     | …Webchat-orçamento     | channel | message_contains:orçamento | move to qualificando |

A second message on the **WhatsApp** channel of the same tenant
containing "preço" — but with no channel-scoped rule for that number
— would instead resolve to `preço → negociacao` (team `sales` wins
over tenant default), satisfying AC#3 of [SIN-62197](/SIN/issues/SIN-62197):
the Webchat rule never bleeds into WhatsApp because the channel
scope id does not match.

### D4 — Hexagonal boundary

`internal/funnel/rules/` contains:

- `rule.go` — `Rule` value type, `ScopeType` enum, `Trigger`,
  `TriggerKind`, `ActionKind`, action payload value objects.
- `resolver.go` — `Resolver`, `ResolveInput`, `ResolvedRule`, the
  cascade algorithm. **No SQL, no HTTP, no SDK.**
- `repository.go` — the `Repository` port:

```go
type Repository interface {
    // ListByScopeAndTriggers returns rules in the (tenant_id, scope_type, scope_id) bucket
    // whose (trigger_kind, trigger_key) is in the input set, filtered to enabled=true.
    // The adapter MUST NOT inject ordering across scopes — the resolver owns cross-scope
    // precedence.
    ListByScopeAndTriggers(
        ctx context.Context,
        tenantID uuid.UUID,
        scopeType ScopeType,
        scopeID uuid.UUID,
        triggers []Trigger,
    ) ([]Rule, error)
}
```

`internal/adapter/db/postgres/funnel/rules_repo.go` is the Postgres
adapter — the only file that imports `database/sql` (or the project's
`pgxpool` wrapper) for funnel rules. The existing
`lint-postgres-adapter-tests` and `forbidimport`/`no-sql-in-domain`
analyzers cover this boundary automatically; no new lint rule is
required.

The **consumer** lives at `internal/funnel/automation/` (placeholder
for Fase 4 C9). It subscribes to `funnel.conversation_moved` (the
existing event in
[`internal/funnel/domain.go`](../../internal/funnel/domain.go)) plus
the inbound-message event, derives the candidate triggers, calls
`Resolve`, and executes each `ResolvedRule.Rule.Action` via the
existing funnel-transition use-case. **The resolver never executes
actions itself.**

### D5 — Audit and observability

Every `Resolve` call emits one structured audit record per *adopted*
rule, before the action runs:

```json
{
  "event": "funnel.rule.matched",
  "ts": "2026-05-17T18:24:11.456Z",
  "tenant_id": "01HXYZ...",
  "channel_id": "01HXYZ...",
  "team_id": "01HXYZ...",
  "conversation_id": "01HXYZ...",
  "rule_id": "01HXYZ...",
  "source_scope": "channel",
  "trigger_kind": "message_contains",
  "trigger_key": "preço",
  "action_kind": "move_to_stage",
  "action_payload": {"stage_key": "webchat-lead"}
}
```

When a rule is **dropped** by the cascade (a less-specific rule was
overridden by a more-specific one), the resolver emits a single
debug-level structured log per dropped rule:

```json
{
  "event": "funnel.rule.shadowed",
  "tenant_id": "01HXYZ...",
  "dropped_rule_id": "01HXYZ...",
  "dropped_scope": "tenant",
  "winning_rule_id": "01HXYZ...",
  "winning_scope": "channel",
  "trigger_kind": "message_contains",
  "trigger_key": "preço"
}
```

This event is **not** an audit record. It is operational telemetry
that the admin UI uses to render the "this tenant rule is shadowed
by a channel-scope rule on the same trigger" warning. The audit log
for the runtime decision stays narrow: only adopted rules fire and
only adopted rules are audited.

Latency metric `funnel.rules.resolve_ms` ships day one (Prometheus
histogram, p50/p95/p99 buckets), parallel to `aipolicy.resolve_ms`
from [ADR 0042](./0042-policy-cascade.md) §D5.

### D6 — Feature flag and rollback

The automation consumer reads a **tenant-scoped boolean flag**
`funnel.rules.enabled` (default `false`). When the flag is off, the
consumer subscribes and resolves rules **for telemetry**, but never
executes the action. The flag is the rollback knob:

- **Forward rollout**: enable the flag per tenant after the operator
  has reviewed the admin UI's "what would have happened" diff
  (sourced from the shadow-resolve telemetry).
- **Backward rollback**: flip the flag to `false` for one tenant or
  for all tenants. The resolver still runs (so we keep the
  observability), but no transitions or tags are written.

The flag belongs in the existing per-tenant `feature_flag` table
(no new schema). The migration that adds the `funnel_rule` table
ships **without** seeding any rows, so a tenant in `enabled=false`
state with no rules is the inert default — the safest possible
deploy.

### D7 — Verification plan

- **Unit tests** in `internal/funnel/rules/resolver_test.go`,
  table-driven, table covers:
  - empty input scopes (only tenant scope set)
  - one trigger, no rules → empty result
  - one trigger, only tenant rule → adopt tenant
  - one trigger, channel+tenant rules → adopt channel, drop tenant
  - one trigger, team+tenant rules (no channel) → adopt team, drop tenant
  - one trigger, channel+team+tenant rules → adopt channel, drop both
  - two triggers, mixed scope coverage (AC#3 worked example above)
  - position tiebreak within same scope (deterministic ordering)
  - disabled rule excluded
  - cross-tenant isolation (rule in tenant B not returned for tenant A)
- **Integration test** in the C9 consumer package
  (`internal/funnel/automation/...`): drives the real Postgres
  adapter (per [Quality bar rule 5](../../AGENTS.md): no DB mocking),
  publishes a `funnel.conversation_moved` event, asserts the action
  ran and the audit row exists with the expected
  `(rule_id, source_scope)`.
- **Coverage gate**: `internal/funnel/rules/` package coverage must
  exceed the standing 85% bar.

## Consequences

Positive:

- **Least surprise**: the operator who wrote a channel-level rule
  sees it override the tenant-level rule for the same trigger,
  exactly as they expect. The cascade is documented and visible in
  the admin UI ("this tenant rule is shadowed by 2 channel rules").
- **Composable across scopes**: distinct triggers from different
  scopes all fire. A tenant-default campaign-click rule still moves
  the conversation to `novo`, even when a channel-level
  message-keyword rule simultaneously moves it after the first
  inbound message. This is the cross-scope flexibility 0042's
  all-or-nothing model cannot express.
- **Hexagonal**: pure resolver + one repository port + adapter, same
  shape as [ADR 0042](./0042-policy-cascade.md) §D7. The C9 consumer
  is the only side-effect surface; the resolver is trivially
  unit-testable.
- **Observability**: per-rule audit + shadow telemetry. Operator can
  ask "why did this conversation move?" and get a single rule id and
  source scope back.
- **Reversibility**: feature flag, no-rule default, no schema lock-in.
  Rolling back means flipping a boolean per tenant.

Negative / costs:

- Three repository calls per resolve. Same envelope as 0042 (also
  three) but with a wider result set per call. Index-only scans on
  the `(tenant_id, scope_type, scope_id) WHERE enabled` partial
  index; expected sub-ms.
- The cascade-and-deduplicate step is more complex than 0042's
  short-circuit. The deduplication key is a small struct; not a
  performance concern, but it is more code to read.
- Operators must learn that **same trigger across scopes** behaves
  cascade-style, while **different triggers across scopes** all
  fire. The admin UI must surface this distinction explicitly (the
  shadow warning is the primary mitigation).

Risk residual:

- **Operator authors a tenant-scope rule and a channel-scope rule
  with the same trigger key but different actions, expecting both
  to fire.** They will not — the channel rule shadows the tenant
  rule. The admin UI displays the shadow warning on save and the
  resolver emits `funnel.rule.shadowed` telemetry so the dashboard
  can highlight tenants with persistent shadows.
- **Two operators on the same team author conflicting same-scope
  same-trigger rules.** The UNIQUE constraint prevents duplicate
  rows at the schema level — the second `INSERT` fails. The admin
  UI must surface this as a friendly "an existing rule already
  targets this trigger; replace it?" prompt rather than a raw
  database error.
- **Trigger canonicalisation drift.** The cascade only collapses
  conflicts when `trigger_key` is equal byte-for-byte. The use-case
  must canonicalise keys (lower-case, NFC, trim) before constructing
  the rule row *and* before constructing the candidate `Trigger`.
  This is a single helper in the domain package; a unit test pins
  the canonicalisation contract.

## Alternatives considered

### Option A — Additive merge (all matching rules fire, no precedence)

Concatenate rules from every scope. If a tenant-level rule and a
channel-level rule both match the same trigger, both actions run.

Rejected because:

- Operator intent is violated when both actions are
  `move_to_stage`. A conversation cannot be in two stages
  simultaneously; the second move overwrites the first. The order
  of moves becomes a function of database scan order or event
  processing order — non-deterministic at audit time.
- Even when both actions are `tag_conversation`, the implicit
  "tenant default still wins despite a channel override" outcome is
  precisely the surprise we are designing against. Operators
  expect specificity to dominate.
- AC#3 of [SIN-62197](/SIN/issues/SIN-62197) explicitly rules this
  out: a WhatsApp channel must **not** inherit a Webchat channel's
  rule. Additive merge satisfies the negative ("do not bleed
  cross-channel") but fails the positive ("override correctly when
  scope matches").

### Option B — Last-write-wins (`updated_at` decides)

When two rules across scopes share a trigger, the more recently
updated row wins.

Rejected because:

- Updating a tenant-default rule (a low-frequency operation, e.g.
  fixing a typo) would silently *override* a channel-scope rule that
  has not been touched in months. The "newer wins" semantics tie
  precedence to mutation timing rather than scope, which is exactly
  the inverse of operator intent.
- Audit forensics become a multi-hop story: "which rule applied
  depended on which row was edited last." The lens **observability
  before optimisation** rules this out — we want precedence to be a
  property of the cascade, not of the editor's keyboard.

### Option C — Single flat ruleset per tenant (no scope hierarchy)

One `funnel_rule` table without `scope_type` / `scope_id`; rules
match on whichever subset of `(channel_id, team_id)` they target.

Rejected because:

- Encodes specificity into the rule predicate rather than into the
  schema. Two rules — "channel = Webchat, trigger = preço, action =
  webchat-lead" and "trigger = preço, action = qualificando" — both
  match a Webchat message, and the conflict-resolution rule
  ("longer predicate wins"? "narrower channel set wins"?) is
  ad hoc and hard to explain.
- Loses the partial-index benefit. Postgres cannot index "no
  scope" as efficiently as "scope_type = 'tenant'". The
  `(tenant_id, scope_type, scope_id) WHERE enabled` index in D1 is
  the boring solution.
- Loses parity with [ADR 0042](./0042-policy-cascade.md). Two
  cascades in the same codebase should look the same; cognitive
  load for operators and for engineers stays low.

### Option D — Cascade with all-or-nothing override (verbatim 0042 model)

Same as 0042: the most-specific scope wins, and a more-specific
scope's *full ruleset* replaces any less-specific scope's ruleset.

Rejected because:

- A single channel-level rule on one trigger would shadow **all**
  tenant-default rules (campaign clicks, idle conversations,
  every keyword). Operators do not want to copy the entire tenant
  default into every channel override just to add one extra
  channel-specific keyword.
- The 0042 model fits "one decision per call" (which policy do we
  use). The funnel model is "which actions does this event trigger,
  potentially many" — they are different shapes of decision, and
  the cascade must be per-trigger, not per-scope.

### Option E — Channel > tenant > team (different order)

A tenant default outranks a team override.

Rejected because:

- A team is organisationally **inside** the tenant — squad-specific
  preferences must override the tenant baseline, not the other way
  round. Reversing the order lets a tenant-wide knob silently
  weaken a team's stricter rule, which is the opposite of what the
  operator squad wants.
- Channel > team > tenant is also the order [ADR
  0042](./0042-policy-cascade.md) §C uses. Two cascades, same order,
  one mental model.

## Lenses cited

- **Least surprise.** Channel rules override tenant rules on the
  same trigger; operators see exactly what their channel-level
  authoring intent did.
- **Least privilege.** Distinct triggers do not bleed across scopes
  — a channel-level keyword rule does not unilaterally suppress an
  unrelated tenant-level campaign-attribution rule.
- **Hexagonal / ports & adapters.** Pure resolver, Postgres adapter
  behind a port, audit + execution in the consumer use-case.
- **Reversibility & blast radius.** Per-tenant feature flag turns
  the consumer into a no-op without removing data. Resolver still
  runs for telemetry.
- **Observability before optimisation.** No cache yet; latency
  metric ships day one. Shadow telemetry surfaces "rules that are
  silently overridden" so admins can audit.
- **Boring technology budget.** One table, one partial index, three
  point lookups, same shape as the existing `ai_policy` cascade.

## Out of scope

- A cache layer in front of `Repository`. Deferred until
  `funnel.rules.resolve_ms` exceeds the alert threshold. [ADR
  0042](./0042-policy-cascade.md) §D5 lists the same deferral.
- Time-window scopes ("only fire this rule during business hours").
  Out of scope for Fase 4; a future ADR can add a scheduling layer
  above this resolver without changing the cascade shape.
- Cross-tenant or master-level rules (a CRM operator imposes a rule
  on all tenants). Out of scope; master-level controls ship in a
  later phase alongside other master surface work.
- A rule "priority" / "weight" knob that overrides the cascade
  ("this tenant rule is more important than any channel rule").
  Explicitly rejected: it reintroduces the
  precedence-by-arbitrary-knob problem Option B was rejected for.
- Action types beyond `move_to_stage` and `tag_conversation`
  (webhook fan-out, outbound message templates). A future ADR may
  add them; the resolver shape does not need to change.

## Rollback

The migration `funnel_rule` table is additive: dropping the table
is a backward-compatible operation as long as the consumer has been
disabled via `funnel.rules.enabled = false` for every tenant first
(no live writes against the table). The migration's `down` script
drops the table and the partial index; no FKs from other tables
reference it.

Operationally, rollback is a two-step sequence:

1. Flip `funnel.rules.enabled = false` for every tenant (per-tenant
   feature flag — the existing admin tooling already supports bulk
   updates). The consumer keeps subscribing and resolving for
   telemetry but stops executing.
2. If a further code rollback is needed, revert the C9 consumer
   PR; the resolver and the schema can stay in place harmlessly,
   since no caller invokes them.

Only after Fase 4 is fully retired (no plausible path back) does the
`funnel_rule` table get a destructive migration. At that point the
shadow telemetry has already shown zero ongoing usage.
