# ADR 0085 — Fork ↔ upstream reconcile (reset fork main and re-land in batches)

- Status: accepted
- Date: 2026-05-09
- Deciders: CEO (Pericles), CTO
- Tickets: [SIN-62384](/SIN/issues/SIN-62384) (decision + execution),
  [SIN-62373](/SIN/issues/SIN-62373) (disjoint-roots confirmation),
  [SIN-62292](/SIN/issues/SIN-62292) (two-tier deploy model that exposed the
  problem), [SIN-62510](/SIN/issues/SIN-62510) (this ADR)

## Context

The CRM repository runs a two-tier deploy model: engineers open PRs against
`ia-dev-sindireceita/crm` (the "fork") and the CTO opens a deploy PR
`fork:main → pericles-luz/crm:main` (the "upstream") for the CEO to merge. The
model assumes that fork and upstream share a git ancestor so each deploy PR
shows a clean fork-only delta.

That assumption broke. The fork was set up with its own initial commit instead
of being a true `git fork` of `pericles-luz/crm`, and both sides accumulated
independent work on `main`:

- Fork-only: master MFA chain (SIN-62342 PRs #15–#23), CSRF foundations,
  password hashing helpers, agent-developed feature work.
- Upstream-only: F43–F49 chain (slugreservation, media upload/serve,
  customdomain), ADRs 0079/0080, and ad-hoc work merged directly by Pericles.

Confirmed 2026-05-08 in [SIN-62373](/SIN/issues/SIN-62373):

```
git merge-base ia-dev-sindireceita/crm/main pericles-luz/crm/main
# → empty
```

Two `main` branches with disjoint roots cannot be deploy-PR'd cleanly. Every
attempted `fork:main → upstream:main` PR would show a content union of ~400+
files (everything from both sides) rather than the fork-side delta. One-off
workarounds (cross-fork PRs based on upstream HEAD, direct upstream pushes via
the shared `ia-dev-sindireceita` GitHub identity) kept individual features
shipping but didn't fix the structural problem, and the gap grew with each
heartbeat.

## Decision

Adopt **Option A — reset fork `main` to upstream `main` and re-land fork-only
work in batches.**

Mechanics, executed 2026-05-09:

1. Tag `main-legacy-pre-reconcile-20260509` on the pre-reset fork `main` SHA
   as an immutable backup. Pushed to `origin` (fork remote) so the tag is
   recoverable from GitHub even if local clones are lost.
2. Force-push `ia-dev-sindireceita/crm:main` to exactly match
   `pericles-luz/crm:main` HEAD (408e90a at execution time). After this step,
   `git merge-base origin/main pericles-luz/main` returns a real commit and
   the two branches share a base.
3. Re-land fork-only work as a sequence of small PRs against the new
   `main`, ordered by dependency. Each batch is a normal CTO-reviewed fork
   PR; the deploy-PR cap on upstream still applies once each deploy ships.

Subsequent deploy PRs (`fork:main → upstream:main`) now show only the fork-side
delta and are reviewable in the usual way.

## Consequences

Positive:

- Future deploy PRs are clean diffs. Pericles can review the delta without
  wading through unrelated upstream history.
- The two-tier deploy model from [SIN-62292](/SIN/issues/SIN-62292) becomes
  structurally sound — agents PR to fork, CTO opens deploy PR, CEO merges.
- The disjoint-roots one-off waivers documented in
  [SIN-62373](/SIN/issues/SIN-62373) (cross-fork PRs targeting upstream from
  fork branches) are no longer required. Fork branches are normal
  fork-of-upstream branches again.
- `main-legacy-pre-reconcile-20260509` preserves the entire pre-reset fork
  history. Any fork-only commit that turns out to have been missed during
  re-landing can be located in that tag and cherry-picked.

Negative / costs:

- Force-push to `main` is a destructive operation on a shared branch. We
  accepted this because: (a) the backup tag is recoverable, (b) no engineer
  had open work on fork `main` at the reset moment that wasn't already
  represented on upstream, (c) CEO sign-off was explicit before execution.
- Re-landing fork-only work as batches is engineering effort spread over
  multiple heartbeats. The cost is amortized against every future deploy PR
  being clean.
- Tooling that hard-coded the pre-reset SHA of fork `main` (none known) would
  need to be rebased. None surfaced.

Operational rules following the reset:

- The backup tag `main-legacy-pre-reconcile-20260509` is **immutable**. Do not
  delete or move it. It is the only recovery path for the pre-reset history.
- Engineers branch from current `origin/main` as usual. Branches that were
  authored against the pre-reset fork `main` and haven't merged must be
  rebased onto the new base before opening or re-opening their PR.
- The CTO is responsible for verifying that each re-landing batch is
  represented either on upstream or in the live fork PR queue before marking
  the reconcile complete.

## Alternatives considered

### Option B — `git merge --allow-unrelated-histories`

Merge fork `main` into upstream `main` (or vice versa) with
`--allow-unrelated-histories`, producing a single merge commit that joins the
two roots.

Rejected because:

- The resulting merge commit's diff is the full content union — ~400+ files
  changed, half of which are unrelated to any single feature. Reviewing it
  meaningfully is impossible, and Pericles' merge tooling has a hard time
  with diffs that large.
- File-level conflicts where both sides modified the same path (e.g.,
  `go.mod`, `Makefile`, top-level config) would need ad-hoc resolution with
  no clean test of "which version is right". Each conflict resolution
  becomes an unaudited decision baked into the merge commit.
- The merge produces a single point of failure: if anything is wrong with the
  conflict resolution, the entire reconcile must be redone.
- Lens **reversibility & blast radius**: the merge commit is hard to revert
  and has a large blast radius. Option A's force-push is reversible via the
  backup tag.

### Option C — Branch flip + default-branch rename

Create a new branch `main-v2` on the fork seeded from upstream, re-land
fork-only work onto `main-v2`, then rename `main-v2 → main` via GitHub's
default-branch rename UI. Original `main` is kept as a historical branch.

Rejected because:

- Adds a second permanent branch on the fork forever. Every clone, CI config,
  Dependabot config, and protection rule needs to know about both. Operational
  overhead with no upside vs. Option A.
- Default-branch renames in GitHub force every open PR's `base` to be
  updated, every clone to re-track, and break any external integration that
  hard-codes the branch name. Option A avoids all of that — `main` stays
  `main`, only its tip moves.
- Lens **boring technology budget**: Option A uses standard git (tag +
  force-push). Option C introduces GitHub-specific default-branch rename
  ceremony for no recovery benefit Option A doesn't already provide via the
  backup tag.

## Lenses cited

- **Reversibility & blast radius.** The force-push is recoverable via
  `main-legacy-pre-reconcile-20260509`. Option B's merge commit is not
  reversible in any clean way once it ships through deploy. Option C's branch
  rename touches every consumer of the branch name.
- **Boring technology budget.** Option A is plain git: tag, force-push, open
  PRs. Option B requires `--allow-unrelated-histories` (a flag specifically
  designed to override git's safety check that two histories should be
  joined). Option C requires GitHub-specific default-branch ceremony.
- **Defense in depth.** Backup tag + CEO sign-off + restricted fork access
  combine to make the destructive step recoverable on three axes: data
  (tag), authorization (sign-off), and access (only CTO/CEO can force-push
  fork `main`). No single failure loses the pre-reset history.

## Rollback

If the reset turns out to have been a mistake (e.g., a fork-only branch is
discovered to have unmerged work that was missed):

1. `git checkout main-legacy-pre-reconcile-20260509`
2. Force-push that SHA back to `ia-dev-sindireceita/crm:main`
3. Re-evaluate before attempting reset again

Rollback requires CEO sign-off, same as the original reset.

## Out of scope (decisions separately)

- The per-batch re-landing schedule and contents are tracked as child issues
  of [SIN-62384](/SIN/issues/SIN-62384), not in this ADR.
- The two-tier deploy model itself is documented in
  [SIN-62292](/SIN/issues/SIN-62292); this ADR fixes a precondition for that
  model, not the model.
- Branch protection rules on fork `main` (force-push protection going
  forward, required reviewers, required status checks) are out of scope here
  — the reset required force-push capability and protection rules will be
  tightened in a separate change once the re-landing is complete.
