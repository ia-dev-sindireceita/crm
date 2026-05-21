# ADR 0101 — ConsentRegistry: generic LGPD consent ledger; `ai_policy_consent` stays specialized

- Status: Proposed
- Date: 2026-05-21
- Deciders: CTO
- Drives: [SIN-63185](/SIN/issues/SIN-63185) (this ADR), Fase 6 PR2 LGPD work
- Builds on: [SIN-62927](/SIN/issues/SIN-62927) (migration 0101 `ai_policy_consent`),
  [SIN-62928](/SIN/issues/SIN-62928) (`aipolicy.ConsentService` and Fase 3 decisão #8),
  [ADR 0042](./0042-policy-cascade.md) (AI policy scope cascade).

## Context

Fase 6 needs an LGPD-grade consent ledger for **non-AI** purposes: a
tenant operator (or end-customer) must be able to grant and revoke
consent for `terms_of_service`, `privacy_policy`, marketing
communications, and analytics cookies, with a clear audit trail
(timestamp, IP, user-agent) and queryable history.

The `ai_policy_consent` table already exists (migration 0101 / Fase 3
decisão #8). It records the operator's acceptance of an **anonymized
payload preview** before the first IA call for a given (tenant, scope)
scope and is keyed by `(tenant_id, scope_kind, scope_id)`. Each row
carries:

- `payload_hash` — SHA-256 of the preview the operator accepted.
- `anonymizer_version`, `prompt_version` — version pivots that force a
  re-consent flow when either rolls forward.
- A single active row per scope (UPSERT-in-place; the gate's
  cascade-on-version-bump invariant relies on the UNIQUE
  `(tenant_id, scope_kind, scope_id)`).

`ai_policy_consent` therefore answers a fundamentally different
question than the new LGPD ledger:

| Aspect                 | `ai_policy_consent` (AI-specific)            | `consent_record` (generic LGPD)            |
|------------------------|----------------------------------------------|--------------------------------------------|
| Subject of the consent | Operator within a tenant, per IA scope       | User / contact / tenant, per purpose       |
| Identity key           | `(tenant, scope_kind, scope_id)`             | `(tenant, subject_type, subject_id, purpose, version)` |
| Versioning pivot       | `anonymizer_version` + `prompt_version`      | document `version` (free-form, e.g. "v2026-05") |
| Cardinality            | One active row per scope; UPSERT in place    | One row per version; history retained      |
| Revocation semantics   | Implicit — next IA call re-prompts on mismatch | Explicit — `Revoke` records `revoked_at` + reason |
| LGPD audit row         | Captured indirectly via `ai_policy_audit`    | Mandatory per Record/Revoke into `audit_log_data` (IP + UA) |

These shapes are not unifiable without distorting one of them.
Forcing `ai_policy_consent` into a generic per-event ledger loses the
"cascade on anonymizer/prompt bump" invariant; forcing the generic
ledger into per-scope UPSERT loses history and revocation evidence
that LGPD requires.

## Decision

1. Introduce a new domain primitive `internal/iam/consent` exposing
   the `ConsentRegistry` port (Record / Latest / History / Revoke) and
   a Postgres adapter `internal/iam/consent/pgconsent` backed by a new
   table `consent_record` (migration 0107).
2. `consent_record` covers **non-AI** LGPD purposes only:
   `terms_of_service`, `privacy_policy`, `marketing`,
   `cookies_analytics`. The CHECK constraint is wire-stable: adding
   a purpose requires a follow-up migration.
3. `ai_policy_consent` and `aipolicy.ConsentService` remain unchanged.
   They continue to gate IA calls per (tenant, scope_kind, scope_id),
   using `payload_hash`, `anonymizer_version`, and `prompt_version`.
   No data migration moves rows between the two tables.
4. The integration with `ai-policy` is **conceptual, not structural**.
   Both primitives share the LGPD posture (operator-attributable,
   tenant-scoped, auditable) but live behind separate ports because
   their invariants differ. A future ADR may revisit collapsing them
   if a real shared use-case emerges; today there is none.
5. Every `Record` and `Revoke` call writes one `audit_log_data` row
   with `event_type` in `{'consent_grant','consent_revoke'}`,
   carrying the caller's IP and user-agent in `target`. The migration
   extends the existing `audit_log_data.event_type` CHECK clause to
   accept these literals.

## Why not migrate `ai_policy_consent` into `consent_record`?

- The version pivot is different: ai_policy_consent's version is the
  pair (anonymizer, prompt); consent_record's is a free-form document
  version string. A unified `version` column would require encoding
  the pair into a string, which would break cheap "list every consent
  under anonymizer X" queries the gate needs.
- The cardinality is different: ai_policy_consent stores one active
  row per scope (UPSERT in place); a unified ledger stores one row per
  version. Migrating means choosing between (a) keeping ai_policy
  rows UPSERT-in-place and breaking the unified history invariant or
  (b) creating one consent_record per IA call, which is far more
  expensive and changes the cascade-on-version-bump semantics the
  gate relies on.
- The blast radius is real: the IA gate (Fase 3) is in production
  paths. A migration that converted scope-keyed UPSERT into
  version-keyed APPEND silently changes "has the operator already
  accepted this preview?" into "did the operator ever accept any
  preview?". That would weaken the LGPD posture rather than improve
  it.

The lower-risk path is to keep both, document the relationship, and
revisit only if a concrete unification use-case shows up. This ADR
records that choice.

## Consequences

- A new table, port, and adapter ship in this PR alongside the
  migration. Cmd-server wiring follows in a follow-up PR.
- Callers that need AI-specific consent (the IA gate) continue to
  depend on `aipolicy.ConsentService`. Callers that need generic LGPD
  consent (ToS acceptance UI, marketing opt-in, cookie banner)
  depend on `consent.ConsentRegistry`.
- The `audit_log_data` CHECK clause grows by two literals
  (`consent_grant`, `consent_revoke`). Future event names are added
  the same way (migration + corresponding `DataEvent` constant +
  `IsKnown` map entry).
- Reversibility: migration 0107 has a down step that drops the
  consent_record table and rolls the `audit_log_data` CHECK clause
  back to its 0083 form. The down step is non-destructive only when
  no rows exist in `consent_record`; it is documented as a developer-
  environment rollback path, not a production reverse step.

## Alternatives considered

1. **Replace `aipolicy.ConsentService` with `ConsentRegistry`**.
   Rejected for the reasons in "Why not migrate" above.
2. **Add `payload_hash` and version columns to `consent_record`**.
   Rejected: every non-AI consent row would carry NULL columns it
   does not need, the CHECK constraints would have to branch by
   purpose, and the LGPD-purpose audit reader would have to ignore
   the AI-only columns. Cheaper to keep the two domains separate.
3. **Single port with two adapters**. Rejected for now: the method
   signatures diverge (AI consent's `HasConsent` takes anonymizer/
   prompt versions; generic consent's `Latest` does not), so a single
   port either loses type safety or grows method-per-purpose. ADR
   revisitable when a third primitive arrives and the abstraction
   pays for itself.
