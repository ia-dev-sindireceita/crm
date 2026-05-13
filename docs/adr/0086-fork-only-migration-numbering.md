# ADR 0086 — Fork-only migration numbering convention (`0076_*` start)

- Status: accepted
- Date: 2026-05-13
- Deciders: CTO
- Tickets: [SIN-62534](/SIN/issues/SIN-62534) (this ADR),
  [SIN-62514](/SIN/issues/SIN-62514) (ruling that established the
  convention), [SIN-62510](/SIN/issues/SIN-62510) (ADR 0085 — reset
  precondition), [SIN-62384](/SIN/issues/SIN-62384) (reconcile parent;
  source of the "do NOT renumber upstream-side migrations" rule),
  [SIN-62515](/SIN/issues/SIN-62515) / [SIN-62516](/SIN/issues/SIN-62516) /
  [SIN-62525](/SIN/issues/SIN-62525) (re-landing batches that apply this
  convention)

## Context

[ADR 0085](/SIN/issues/SIN-62510) reset `ia-dev-sindireceita/crm:main` to
match `pericles-luz/crm:main` so the two-tier deploy model produces clean
diffs. The reset rebased upstream's full migration chain — including
`migrations/0001_roles…0007_create_audit_log` and the
`0075a..0075d_gc_jobs` block — onto fork `main`. Fork-only migrations
that the agent fleet had previously authored against the pre-reset fork
(`0008_account_lockout` … `0015_drop_audit_log`) need to be re-landed as
part of the [SIN-62384](/SIN/issues/SIN-62384) batches.

Reusing the original `0008..0015` numbers is unsafe. Upstream already
occupies `0008..0011` with `0008_master_login_attempts`,
`0009_master_mfa_setups`, `0010_login_attempts_retention`, and
`0011_session_token_pepper`. Re-landing the fork-only files under those
numbers would either overwrite the upstream files (silently corrupting
upstream-authored DDL) or fail at `migrate up` time on a fresh database
because two files claim the same monotonic id.

The reconcile parent issue [SIN-62384](/SIN/issues/SIN-62384) carries the
broader rule: **fork-side work shifts to clear upstream slots; upstream
files are never renumbered.** That rule needs a concrete numbering policy
so re-landing engineers and the CTO reviewer can agree on the chosen
range without re-deriving it from first principles each PR.

The applicable ruling was issued in
[SIN-62514](/SIN/issues/SIN-62514#comment) on 2026-05-13. This ADR
codifies that ruling so the convention is durable in the repository and
not just in a Paperclip thread.

## Decision

Fork-only re-landed migrations start at **`0076_*`**, immediately after
upstream's `0075d_gc_jobs`. Upstream-side migration files are never
renumbered.

Concretely:

1. The fork-only `migrations/*.sql` files re-landed by
   [SIN-62515](/SIN/issues/SIN-62515),
   [SIN-62516](/SIN/issues/SIN-62516), and
   [SIN-62525](/SIN/issues/SIN-62525) are renamed during re-land so their
   leading number shifts from `0008..0015` to `0076..0086`. The semantic
   order of the legacy chain is preserved.
2. The slots `0012..0074` are **reserved for future upstream growth**.
   Fork-only work does not consume any number in that range, even if
   upstream has not yet landed migrations there. This buffer absorbs
   ordered insertions on Pericles' side without forcing a second
   fork-side renumber on the next pull.
3. Upstream-authored migrations keep their existing numbers
   (`0001..0007`, `0008..0011`, `0075a..0075d`, and any future
   `0012..0074` or `≥0087` upstream work). Fork PRs **must not** rename
   or renumber an upstream-authored migration file. If upstream changes
   one, that change comes in via the normal upstream pull and overwrites
   the fork copy.
4. Within the re-landed `0076..0086` block, the engineer doing the
   re-land chooses the final ordering of the legacy multi-`0009`,
   multi-`0010`, multi-`0011` files, preserving each file's original
   semantic position. The mapping below is the canonical assignment from
   the SIN-62514 ruling; engineers may swap adjacent positions inside a
   single legacy ordinal (e.g. `0077_master_mfa` ↔ `0078_app_audit_role`)
   if a dependency between them requires it, but `0076_*` and `0086_*`
   are fixed.

### Legacy → re-land mapping

| Legacy name                                       | Re-land name                                       |
| ------------------------------------------------- | -------------------------------------------------- |
| `0008_account_lockout.{up,down}.sql`              | `0076_account_lockout.{up,down}.sql`               |
| `0009_master_mfa.{up,down}.sql`                   | `0077_master_mfa.{up,down}.sql`                    |
| `0009_app_audit_role.{up,down}.sql`               | `0078_app_audit_role.{up,down}.sql`                |
| `0010_master_grants.{up,down}.sql`                | `0079_master_grants.{up,down}.sql`                 |
| `0010_master_session.{up,down}.sql`               | `0080_master_session.{up,down}.sql`                |
| `0011_session_activity.{up,down}.sql`             | `0081_session_activity.{up,down}.sql`              |
| `0011_session_csrf_token.{up,down}.sql`           | `0082_session_csrf_token.{up,down}.sql`            |
| `0012_split_audit_log.{up,down}.sql`              | `0083_split_audit_log.{up,down}.sql`               |
| `0013_tenant_audit_data_retention.{up,down}.sql`  | `0084_tenant_audit_data_retention.{up,down}.sql`   |
| `0014_app_audit_role_split.{up,down}.sql`         | `0085_app_audit_role_split.{up,down}.sql`          |
| `0015_drop_audit_log.{up,down}.sql`               | `0086_drop_audit_log.{up,down}.sql`                |

Mechanical notes:

- The renames are pure `git mv` operations. Migration SQL bodies do not
  reference their own file numbers, so no SQL edits are required to
  apply the convention.
- Each batch verifies the chain end-to-end with the project's migration
  runner (`cmd/migrate` against a fresh stg database) on both `up` and
  `down` paths before opening a PR.
- One PR per re-landing batch (SIN-62515 / SIN-62516 / SIN-62525). Do
  not bundle the fork-only chain into a single mega-PR — it would break
  the deploy-PR cadence.

## Consequences

Positive:

- A single monotonically increasing chain in `migrations/` survives the
  ADR 0085 reset. Engineers reading the directory see a clean sequence
  without parallel numbering namespaces or suffix tricks
  (`0008b_…`, `0008-fork_…`).
- Fork-only work is visually distinguishable: any file numbered `≥0076`
  is fork-only until upstream eventually catches up. This makes the
  deploy-PR review easy — Pericles can ignore upstream-authored numbers
  he already saw.
- Future upstream pulls can introduce migrations in the reserved
  `0012..0074` range without forcing the fork side to renumber. Until
  upstream consumes `0075`, the buffer cost is the cardinality of unused
  numbers, which is free.
- Upstream-authored migrations stay byte-identical across the reconcile.
  No upstream file changes blob SHA as a side effect of fork-side work.

Negative / costs:

- The `0012..0074` gap is intentionally permanent on the fork side until
  upstream fills it. New readers may wonder why `migrations/` jumps from
  `0011_*` to `0075a_*`. The header of `migrations/` and this ADR
  explain it; no additional code is required.
- If upstream ever lands beyond `0075d` faster than expected and reaches
  `0076`, a second fork-side renumber is required. This is acceptable
  because (a) upstream landing 60+ migrations in a short window is
  unlikely given current cadence, (b) re-renumbering is mechanical
  `git mv`, and (c) the alternative (compact `0012_*`) would force a
  renumber on every upstream pull, not just on rare high-volume bursts.

Operational rules following this ADR:

- New fork-only migrations after the re-landing chain (`0086_drop_audit_log`)
  continue upward from `0087_*`. Do not reset back into the
  `0012..0074` reserve, even if upstream has not landed there.
- When pulling upstream, run `ls migrations/ | sort` and confirm no
  upstream number collides with a fork number. If it does, the fork
  side renumbers (this ADR's rule), not the upstream side.
- The legacy `0008..0015` filenames on
  `main-legacy-pre-reconcile-20260509` are immutable artifacts of the
  pre-reset history. Do not retag, rename, or rewrite that branch.

## Alternatives considered

### Option B — Compact `0012_*` (rejected)

Re-land fork-only files starting at `0012_*` (immediately after the
upstream `0008..0011` block), filling the smallest available slot.

Rejected because:

- The first upstream pull that lands a migration in `0012..0074` would
  collide with a fork file and force a second renumber. Upstream
  already occupies `0075a..0075d` and the `0008..0011` range, so
  `0012..0074` is the natural growth path for Pericles. Sitting in that
  range is a near-certain collision target.
- Lens **reversibility & blast radius.** Compact-at-12 is correct *now*
  and wrong on the next upstream pull. The reset itself was painful
  enough; the convention should not bake in another reset.
- Lens **boring technology budget.** Re-renumbering on every upstream
  pull would require either tooling we don't have or a documented
  per-pull ceremony. Reserving `0012..0074` once is the cheaper option.

### Option C — Suffix-based parallel namespace (rejected)

Keep fork files at their original numbers (`0008_account_lockout` etc.)
and disambiguate from upstream via a suffix (`0008-fork_*`,
`0008b_*`). Migration runners that sort lexicographically would still
apply the chain in a deterministic order.

Rejected because:

- Two parallel numbering namespaces on one directory. Readers can no
  longer scan `migrations/` and trust the leading number as a global
  ordinal. Tooling that splits on `_` would need updating.
- Migration runners pin the migration *name* in the migrations table.
  Renaming `0008_account_lockout` to `0008b_account_lockout` is, from
  the runner's perspective, a brand-new migration — the same SQL body
  would attempt to apply against a database that already has it. The
  re-landing batches need to clean-apply on a fresh stg database, but
  the suffix approach also breaks any environment that previously
  applied the legacy file.
- Lens **least surprise.** A monotonically increasing chain matches
  every other migration convention engineers have seen
  (`golang-migrate`, Rails, Alembic). Adding letters or `-fork`
  suffixes is local trivia future readers must learn.

### Option D — Two `migrations/` directories (rejected)

Split into `migrations/upstream/*.sql` and `migrations/fork-only/*.sql`,
each with its own monotonic chain. The runner concatenates both
directories in a documented order before applying.

Rejected because:

- Requires runner changes. `cmd/migrate` currently points at one
  directory; adding multi-source support is non-trivial and
  introduces a custom convention upstream does not share.
- Lens **boring technology budget.** Stay on stock `golang-migrate` with
  one directory. New plumbing for a numbering question fails the
  smallest-change test.
- Upstream pulls that touch migration filenames would have to be
  manually routed into the right subdirectory. That's exactly the kind
  of routine work where a wrong call silently corrupts the schema chain.

## Lenses cited

- **Reversibility & blast radius.** `0076_*` survives one upstream burst
  of 60+ migrations before re-renumbering becomes necessary; compact-at-12
  fails on the next single upstream addition. The gap is recoverable
  (renumber forward), the collision is not (data loss risk if a fork
  file silently shadows an upstream file with the same number).
- **Boring technology budget.** Stay on stock `golang-migrate` with a
  single `migrations/` directory and integer prefixes. Reject
  suffixes, two-directory runners, and per-pull renumber ceremony.
- **Defense in depth.** Numbering rule + "never renumber upstream"
  ([SIN-62384](/SIN/issues/SIN-62384)) + immutable
  `main-legacy-pre-reconcile-20260509` tag combine so a wrong call on
  one axis (e.g. an engineer reusing a legacy number) is caught by
  another (the upstream file already in place at that number).
- **Least surprise.** Engineers reading `migrations/` see one chain,
  one rule (leading number = global ordinal), one growth direction
  (upward).

## Rollback

If the `0076_*` convention turns out to have been a mistake (e.g.
upstream lands aggressively enough that the gap is exhausted within one
re-landing window), the fork side renumbers forward to the next free
slot above current upstream HEAD, following the same rule that produced
this ADR. The historical re-landed files at `0076..0086` would be
`git mv`d to the new range in a single docs-and-migrations PR; SQL
bodies are not affected.

Rolling back to "compact at 12" is not a supported path — Option B was
rejected for durable reasons, not transient ones.

## Out of scope

- The contents of each re-landing batch are tracked in
  [SIN-62515](/SIN/issues/SIN-62515),
  [SIN-62516](/SIN/issues/SIN-62516), and
  [SIN-62525](/SIN/issues/SIN-62525). This ADR only fixes the numbering
  convention, not which file goes in which batch.
- Branch protection or CI checks that would enforce "no fork PR may
  rename a `migrations/00[01]*_*.sql` file" are a follow-up — the
  convention is currently enforced via CTO review.
- A canonical map between fork-only migrations and the legacy
  pre-reset filenames is captured in the table above and in the
  [SIN-62514 ruling](/SIN/issues/SIN-62514#comment); the
  `main-legacy-pre-reconcile-20260509` tag remains the authoritative
  artifact for the legacy chain.
