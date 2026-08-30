# ADR 0113 — Re-baseline the fork onto `upstream/main` (ADR-0085 *redux*) and re-land only the verified fork-unique delta

- Status: Accepted
- Date: 2026-08-30
- Deciders: CTO
- Sign-off: CTO engineering call. The *execution* half — the force-push of the fork's `main` — is a shared-infra mutation gated on a separate CEO `request_confirmation` on the execution child ([SIN-70316](/SIN/issues/SIN-70316), key `861ea42a`). This ADR decides *what* is dropped and *what* is re-landed; it does not authorize the reset.
- Drives: [SIN-70315](/SIN/issues/SIN-70315) (this ADR + inventory — Child A of [SIN-70312](/SIN/issues/SIN-70312)); gates [SIN-70316](/SIN/issues/SIN-70316) (Child B — execution) and [SIN-70317](/SIN/issues/SIN-70317) (Child C — board sync of 175 PRs).
- Motivated by: [SIN-70310](/SIN/issues/SIN-70310)/[SIN-70312](/SIN/issues/SIN-70312) — Pericles pushed ~644 commits / 175 PRs to `pericles-luz/crm` (upstream, the production source of truth). The fork `ia-dev-sindireceita/crm` diverged with 71 commits of its own, most of which are the **same features re-implemented in parallel** that upstream has since delivered more completely.
- Precedent: [ADR 0085] (first fork re-baseline / disjoint-root reset). This is the second application of the same play.
- Lenses: **Reversibility & blast radius**, **Boring technology**, **Least privilege / defense in depth**, **Test pyramid** (verify-before-drop).

## Context

`git merge-base origin/main upstream/main` = ∅ (disjoint roots, per ADR-0085's
reset). `git rev-list --count`:

- fork is **71 commits ahead** of upstream on shared lineage,
- upstream is **644 commits ahead** of the fork.

This is **not** a fast-forward and **not** a `git merge` candidate — it is a
content reconciliation. Two concrete blockers were raised on the parent:

1. **Migration slot 0134 collision.** The fork placed fork-only work at
   `0130`–`0132` + `0134`; upstream renumbered the same content to `0133`–`0138`
   and added `0138_instagram_oauth_tokens`.
2. **Parallel independent implementations** of the same features exist on both
   sides (WhatsApp outbound, Instagram outbound, Messenger delivered/read
   parity, inbox live-refresh, tenant user-management).

Upstream is the deployed production line and has already absorbed essentially the
entire fork roadmap: ADRs 0100–0110 and 0112 are byte-identical on both sides;
wa_session/whatsmeow, per-channel access (ADR-0109), Instagram/Messenger, and the
user-management migrations all exist upstream and are **more complete** than the
fork's versions (e.g. Instagram: 65 files upstream vs 43 on the fork; upstream
carries `0138_instagram_oauth_tokens`, the fork does not).

## Decision

**Re-baseline the fork onto `upstream/main`** (reset `ia-dev-sindireceita/crm`
`main` to `upstream/main`), then re-land **only** the verified fork-unique delta
on top. Upstream wins every overlapping feature.

### How this dissolves the two blockers

- **Blocker 1 (migration collision) → dissolved, zero renumber.** The four
  fork-numbered migration *pairs* are **SQL-body byte-identical** to their
  upstream renumbered counterparts (verified — the only diffs are the filename
  and numbering references inside leading `--` comments):

  | fork | upstream | body |
  |------|----------|------|
  | `0130_users_deactivated_at` | `0134_users_deactivated_at` | identical |
  | `0131_user_credential_tokens` | `0135_user_credential_tokens` | identical |
  | `0132_audit_log_security_user_events` | `0136_audit_log_security_user_events` | identical |
  | `0134_webhook_0075_series_gap_fix` | `0137_webhook_0075_series_gap_fix` | identical |

  Adopt upstream numbering wholesale (`0133_app_backup_role` … `0138_instagram_oauth_tokens`).
  **The fork-unique re-land set adds no new migration**, so there is **zero
  renumbering** and no `golang-migrate` gap. Blocker 1 is a non-issue after reset.

- **Blocker 2 (parallel impls) → decided, upstream wins.** For every overlapping
  feature (WhatsApp/Instagram/Messenger channels, inbox live-refresh,
  user-management), upstream's implementation is already deployed and more
  complete. Under the **reversibility / blast-radius** and **boring-technology**
  lenses, keeping the shipped-and-proven implementation over a parallel fork
  re-write is the low-risk call. This is an engineering decision, not a product
  ambiguity — there is nothing for the board to disambiguate.

### Verified fork-unique re-land set (closed list)

The authoritative fork-unique surface is the set of files present on `origin/main`
and absent on `upstream/main`: **exactly 23 files** (full `git ls-tree` tree
diff, not just the 71-commit-touched set). They resolve to three re-land groups
plus one drop group:

**RE-LAND — Group A: ADR-0111 image-carried compose deploy stack**
(SIN-66619/66620/66621/66622). Upstream's `stg-deploy.sh` `deploy` verb still
only does "compose pull/up, prune" against **host-resident** compose files;
upstream extracts `/migrations` from the image but **not** the compose / caddy /
unbound / minio carry-set. The fork's image-carried-and-`docker cp`-extracted
deploy is a genuine drift-elimination improvement upstream lacks.
- `docs/adr/0111-image-carried-compose-deploy-artifacts.md`
- `.github/workflows/deploy-artifacts-image-parity.yml`
- `.github/workflows/stg-deploy-wrapper.yml`
- `deploy/scripts/stg-deploy.test.sh`
- `scripts/check-deploy-artifacts-parity.sh` + `scripts/check-deploy-artifacts-parity.test.sh`
- in-place deltas to files that exist on **both** sides: `deploy/scripts/stg-deploy.sh`
  (`deploy`-verb carry-set extraction + fail-closed exits 69/70/71), the
  `Dockerfile` `/deploy` COPY layers, `deploy/compose/compose.stg.yml` if divergent,
  and the `docs/deploy/staging.md` §5 rewrite (SIN-66622).
- **Apply constraint:** re-apply the ADR-0111 delta **on top of upstream's**
  `cd-stg.yml` — upstream carries **newer** action pins (checkout v7.0.0,
  build-push v7.3.0, login v4.6.0 vs the fork's v4.3.1/v6.19.2/v3.7.0). Do **not**
  overwrite `cd-stg.yml` with the fork's copy or the action versions regress.

**RE-LAND — Group B: `master_csrf_rejected_total` metric wire** (SIN-65277).
Upstream's `internal/adapter/httpapi/mastermfa/master_csrf.go` defines the full
`OriginCSRFReason` type + all five reason constants and exposes the `OnReject`
hook, but the composition root leaves it nil — CSRF rejections on the master
operator surface are logged, never counted. The fork adds the metrics-only wire.
- `cmd/server/master_csrf_metrics_wire.go` + `cmd/server/master_csrf_metrics_wire_test.go`
- re-apply the two wire lines in `cmd/server/iam_wire.go` (present on both;
  `newMasterCSRFRejectMetric(...)` → `RequireMasterOriginCSRFConfig.OnReject`).
- **Apply gate:** confirm upstream's `master_csrf.go` still carries the
  `OnReject` field in its config struct; the `delta-compiles-against-upstream-main`
  CI gate catches a signature drift.

**RE-LAND — Group C: impersonation-countdown live timer (CSP-safe static JS).**
Upstream renders the countdown span (`data-impersonation-countdown="true"` +
`data-expires-at`) in `impersonation_banner.go` / `layout.html` but ships **no
JS that animates it** (no `impersonation-countdown.js`, no other file reads the
attribute) — upstream's span is inert with only a `<noscript>` static-time
fallback. The fork adds the external, CSP-safe animator + guard tests.
- `web/static/js/impersonation-countdown.js`
- re-apply the one `<script src="/static/js/impersonation-countdown.js" defer>`
  line in `internal/web/shell/layout.html` (present on both).
- guard tests: `cmd/server/impersonation_countdown_js_static_test.go`,
  `internal/web/master/impersonation_countdown_script_test.go`,
  `internal/web/shell/impersonation_countdown_test.go`.

**RE-LAND (conditional) — Group D: residual fork-only tests.** Not independent
features; reconcile at apply time (keep if they compile against upstream, drop if
superseded by upstream's own coverage):
- `internal/web/master/billing_ledger_labels_test.go` — prod target
  `billing_ledger.go` is **identical** on both sides → safe bonus coverage, keep.
- `internal/web/master/impersonation_feed_test.go` — prod target present on both
  under upstream's layout; keep if it compiles.
- `internal/web/users/handlers_csrf_test.go` (SIN-67232) — upstream's
  `internal/web/users/` exists but its handlers differ from the fork's
  (`billing_ledger_templates.go` and neighbours diverge). This test guards the
  fork's specific hx-headers-on-`<body>` CSRF transport. **Drop unless it
  compiles unmodified against upstream's users surface** — upstream wins the
  users implementation per Blocker 2.

**DROP — Group E: fork-numbered migrations** (8 files, `0130`/`0131`/`0132`/`0134`
up+down). Superseded by upstream `0134`–`0137`, SQL-identical. No re-land.

### go.mod — zero re-land (verified)

Every security dep the fork bumped is at parity with or behind upstream. No
`go.mod` change carries into the re-land set:

| dep | fork | upstream | action |
|-----|------|----------|--------|
| `go-chi/chi/v5` (SIN-68109) | v5.3.0 | v5.3.0 | none |
| `golang.org/x/text` (SIN-68110) | v0.41.0 | v0.41.0 | none (both ahead of the v0.39.0 target) |
| `google.golang.org/grpc` (SIN-68113) | v1.82.1 | v1.82.1 | none |
| `golang.org/x/image` (SIN-69472) | v0.45.0 | v0.45.0 | none |
| Go toolchain (SIN-67147/69472) | go1.26.6 | go1.26.6 | none |

## Consequences

- **Blast radius is the reset itself, nothing else.** After reset+re-land the
  fork is `upstream/main` + 3 small, self-contained groups (deploy stack, one
  metric wire, one static-JS animator). Rollback is `git reset --hard` back to
  the pre-rebase backup tag.
- **Mandatory backup before the reset.** Child B must tag `origin/main` as
  `fork-main-pre-rebase-2026-08-30` (and push the tag) **before** the force-push,
  so the 71-commit lineage is recoverable if a re-land item turns out to matter.
- **The `delta-compiles-against-upstream-main` gate is the safety net** for the
  in-place re-apply deltas (Groups A/B wire lines) — a dangling symbol fails CI
  before merge.
- **No new migration, no renumber, no go.mod change** → the re-land set cannot
  reintroduce Blocker 1 or a dependency regression.
- **The deploy-mechanism delta (Group A) stays on the fork.** Whether ADR-0111's
  image-carried deploy ever goes *upstream* is a separate, future tier-2 deploy
  decision owned by Pericles — not part of this reconciliation.
- **Execution is gated.** The force-push is a shared-infra mutation; it proceeds
  only on the CEO `request_confirmation` tracked on [SIN-70316](/SIN/issues/SIN-70316).

[ADR 0085]: ./0085-fork-rebaseline.md
