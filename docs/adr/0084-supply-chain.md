# ADR 0084 — Software Supply Chain (cosign keyless + SBOM + Dependabot)

- Status: accepted
- Date: 2026-05-07
- Deciders: CTO, SecurityEngineer, Coder (impl)
- Tickets: [SIN-62247](/SIN/issues/SIN-62247) (impl), bundle [SIN-62231](/SIN/issues/SIN-62231), phase [SIN-62199](/SIN/issues/SIN-62199)

## Context

The CRM service ships as a container image to staging (and, in Fase 6, to
production) via the existing GitHub Actions pipeline:

- `.github/workflows/ci.yml` runs the test suite on every PR.
- `.github/workflows/cd-stg.yml` reacts to a green `ci` run on `main`, builds
  the multi-stage distroless image from `Dockerfile`, pushes it to
  `ghcr.io/${{ github.repository_owner }}/crm` (today
  `ghcr.io/ia-dev-sindireceita/crm` — see addendum below on SIN-63281),
  then SSHes into the staging VPS to invoke `/opt/crm/stg/bin/deploy.sh`
  (sourced from `deploy/scripts/stg-deploy.sh`).

That host-side wrapper writes the new digest into `.env.stg`, runs
`docker compose pull && up -d`, and prunes dangling images. The CD SSH key is
locked down with `command="/opt/crm/stg/bin/deploy.sh",no-pty,…` in
`authorized_keys` so this script is the *only* thing the runner can execute on
the host (see `docs/deploy/staging.md`).

Before Fase 6 cuts the first production release we need to guarantee that:

1. Every image consumed at deploy time was produced by **our** GitHub workflow
   on a commit reachable from `main`/a release tag, and has not been replaced
   or backdoored on the registry between push and pull.
2. We can answer "what's inside this image?" — i.e. produce and persist a
   bill of materials per release for vulnerability response and LGPD audits.
3. Known-vulnerable transitive dependencies (Go modules, GitHub Actions,
   container base images) cannot quietly accumulate; CI must surface and
   block them.

Today the build pipeline produces unsigned images and no SBOM, and the deploy
wrapper trusts whatever digest the runner hands it. `govulncheck` already
gates per-PR via `.github/workflows/govulncheck.yml` (SIN-62298), but it only
covers reachable Go CVEs — it does not answer #1 or #2.

## Decision

We adopt a four-part supply-chain baseline, layered onto the existing
pipeline rather than parallel to it.

### 1. Image signing — cosign keyless via GitHub OIDC

- The existing build/push step in `cd-stg.yml` is followed by
  `cosign sign --yes "${IMAGE}@${digest}"` using
  [Sigstore cosign](https://docs.sigstore.dev/cosign/overview) in **keyless**
  mode. The signature is anchored to the **digest** returned by
  `docker/build-push-action`, never the floating tag.
- Authentication uses the workflow's GitHub OIDC token. No long-lived signing
  key is stored anywhere — Sigstore Fulcio mints a short-lived certificate
  bound to the workflow identity for the duration of the run.
- Identity binding (verifier side): the signer certificate's SAN must match
  `^https://github\.com/ia-dev-sindireceita/crm/` (literal dots escaped,
  scoped to the fork `crm` repo specifically — see SIN-63281 addendum) and
  the OIDC issuer must be `https://token.actions.githubusercontent.com`.
  Anything else is rejected, including signatures minted by other repos
  under the same owner. Org migration to `Sindireceita` is tracked in
  [SIN-62322](/SIN/issues/SIN-62322).

### 2. Deploy verify gate

- `deploy/scripts/stg-deploy.sh` (the host-side deploy wrapper invoked over
  SSH) runs `cosign verify` against the requested digest **before** any
  `docker compose pull`/`up`. A missing or invalid signature aborts the
  deploy with a non-zero exit and `compose pull` is never reached.
- The same script is the only sanctioned deploy path: the SSH key in
  `authorized_keys` is locked to `/opt/crm/stg/bin/deploy.sh`, so manual
  `docker compose pull && up -d` is structurally out of policy unless an
  operator with `sudo -u crm-deploy` privilege intervenes (a separate
  break-glass path that requires a logged shell session on the VPS).
- Break-glass: there is intentionally no `--skip-verify` flag. To bypass the
  gate during a Sigstore outage, an operator must edit the deploy script on
  the host (or the source under `deploy/scripts/`) and the change must go
  through PR review. See `Rollback` below.

### 3. SBOM via Syft + cosign attest

- After the image is signed, `cd-stg.yml` runs
  [Syft](https://github.com/anchore/syft) against the same digest to produce:
  - a CycloneDX 1.5 JSON SBOM (`sbom.cyclonedx.json`),
  - and an SPDX 2.3 JSON SBOM (`sbom.spdx.json`).
- Both SBOMs are uploaded as workflow artifacts (90-day retention) so vuln
  triage can pull them without re-running the build.
- Each SBOM is bound to the image with a cosign attestation
  (`cosign attest --predicate ... --type cyclonedx|spdxjson`), so downstream
  consumers can fetch them with `cosign download attestation` against any
  surviving copy of the image.

### 4. Dependency hygiene

- `.github/dependabot.yml` opens weekly PRs (Mondays, America/Sao_Paulo) for:
  - Go modules (`gomod`) at the repo root,
  - GitHub Actions (`github-actions`),
  - Docker base images (`docker`) at the repo root.
- `.github/workflows/security-alerts.yml` queries the GitHub Dependabot
  vulnerability alerts API on every PR and on a daily weekday cron. **Any
  open alert with severity `high` or `critical` fails the workflow.** This is
  a hard gate, not a comment-only signal.
- **Repo prerequisite:** Dependabot alerts must be enabled under
  Settings → Code security and analysis. Without it, the API returns 403
  and the gate fails-closed with an actionable error pointing at this
  ADR — silent pass would be a security regression.

## Consequences

Positive:

- Tampering with an image on `ghcr.io` after publish is detectable — the
  signature won't verify against the new digest, and `stg-deploy.sh` refuses
  to pull.
- An attacker who steals our registry credentials still cannot push a signed
  image, because cosign keyless requires the GitHub OIDC token of an identity
  matching `github\.com/ia-dev-sindireceita/crm/` — and a compromise of any
  *other* repo under the same owner cannot mint a signature that satisfies
  the gate either.
- Every prod-bound image has a CycloneDX + SPDX SBOM tied to its digest,
  satisfying LGPD audit requests ("which version of which library was running
  on date X").
- Known-vulnerable dependencies cannot sit unpatched longer than a weekly
  bump cycle, and a high/critical CVE introduced in a PR fails CI before
  merge.

Negative / costs:

- Extra ~30–60 s per `cd-stg` run for signing + SBOM generation.
- Deploys depend on the Sigstore public-good infrastructure (`fulcio`,
  `rekor`). Outage there blocks deploys; mitigation is the documented
  break-glass below (manual host-side edit, PR-reviewed afterwards).
- The staging VPS now requires `cosign` (>= v2.4) installed in `PATH`. The
  staging runbook (`docs/deploy/staging.md`) documents the install step
  alongside the existing Docker bootstrap.
- Weekly Dependabot PRs require triage discipline. Owner: CTO rotates with
  Coder.

## Alternatives considered

- **Notary v2 / OCI signatures only** — newer, less tooling, no first-class
  GitHub OIDC keyless flow today. Rejected: cosign + Sigstore is the de facto
  standard and Notary v2 adoption inside our toolchain (CI runners, kubectl
  plugins, IDE) is still thin.
- **Manual GPG signing with a long-lived team key** — requires key custody,
  rotation, revocation, and HSM. Rejected: long-lived keys are exactly what
  keyless eliminates, and we have no operational maturity to manage one
  safely.
- **A separate `build-sign.yml` workflow alongside `cd-stg.yml`** — cleaner
  separation of concerns, but produces two parallel build/push pipelines for
  the same image, which doubles GHCR storage churn and creates a window
  where a `cd-stg`-built image is unsigned. Rejected in favour of layering
  signing onto the existing `cd-stg.yml`.
- **Renovate instead of Dependabot** — more flexible grouping, but adds a
  third-party app + secret. Rejected for boring-tech budget; Dependabot is
  native to GitHub and good enough for our dependency surface today.
- **Trivy/Grype scanning of images instead of API-driven alerts** — useful
  but complementary; image scanning is on the roadmap inside ADR 0085
  (container hardening). For dependency-level signal we prefer GitHub's
  first-party alerting because it covers the full transitive graph and the
  alerts have repo-wide visibility for triage.

## Verification (regression tests)

Local checks under `tools/supply-chain/`:

1. `tools/supply-chain/test_deploy_gate.sh` — invokes
   `deploy/scripts/stg-deploy.sh` against fake `docker` and `cosign` binaries
   on `PATH` and asserts:
   - the deploy proceeds (and `compose pull && up -d` are invoked) only when
     `cosign verify` returns 0,
   - the deploy aborts (and `compose pull` is *never* invoked) when
     `cosign verify` returns non-zero.
2. `tools/supply-chain/test_workflow_invariants.sh` — greps `cd-stg.yml`,
   `security-alerts.yml`, and `dependabot.yml` for the cosign sign step,
   both SBOM steps, the cosign attest steps, the >=high severity gate, and
   the three Dependabot ecosystems. Catches accidental removal during
   YAML edits.

CI-only checks (run against real images / real Dependabot alerts):

3. `syft <image>` schema validation (CycloneDX + SPDX) is implicit: the
   `cd-stg` SBOM steps fail closed if Syft cannot emit the format.
4. A test PR introducing a Go module with a known HIGH CVE causes the
   `security-alerts` workflow to fail.

## Operational notes

### SHA pins must be verified against upstream

Every `uses: owner/repo@<sha>  # vX.Y.Z` line in this repo is SHA-pinned per
SIN-62303. When you bump (or first add) such a pin, you MUST verify that the
SHA actually exists in the upstream repository and resolves to the tag you
typed in the comment. A mismatched SHA fails *twice*:

- **At workflow runtime:** GitHub Actions can't resolve
  `unable to find version <sha>` and the job exits before the step starts.
- **In Dependabot:** `latest_commit_for_pinned_ref` calls `git rev-parse`
  inside a fresh clone, hits `error: no such commit <sha>`, and the entire
  ecosystem run is reported as `unknown_error` for that dep — taking the rest
  of that ecosystem's PRs with it on the same job exit. The first
  symptom we saw was [SIN-62324](/SIN/issues/SIN-62324): a typoed
  `sigstore/cosign-installer` SHA blocked the whole `github-actions` job and
  broke `cd-stg` runtime.

To verify a SHA before committing the pin, in any clone with `gh` configured:

```bash
# 1. Confirm the SHA exists in upstream:
gh api repos/<owner>/<repo>/commits/<sha> >/dev/null && echo OK

# 2. Confirm the tag in your `# vX.Y.Z` comment dereferences to that SHA
#    (annotated tags add an indirection through the tag object):
TAG_SHA=$(gh api repos/<owner>/<repo>/git/refs/tags/<tag> --jq '.object.sha')
TAG_TYPE=$(gh api repos/<owner>/<repo>/git/refs/tags/<tag> --jq '.object.type')
if [ "$TAG_TYPE" = "tag" ]; then
  COMMIT_SHA=$(gh api repos/<owner>/<repo>/git/tags/$TAG_SHA --jq '.object.sha')
else
  COMMIT_SHA=$TAG_SHA
fi
echo "$COMMIT_SHA"  # must match the SHA you pinned
```

The `test_workflow_invariants.sh` script intentionally does *not* call the
network — keep this verification step in the PR description (or a runbook
checklist) when bumping action pins.

## Out of scope (handled in sibling ADRs)

- Container hardening (distroless base, non-root UID, read-only rootfs) is
  already partially addressed in the current `Dockerfile` (distroless
  static, non-root UID 65532). Full hardening review is ADR 0085.
- Encryption-at-rest of database backups → separate ticket in the F59 bundle.
- Pre-commit hooks (gitleaks, gofmt) → separate ticket in the F59 bundle.
- `govulncheck` per-PR is already in place via `.github/workflows/govulncheck.yml`
  (SIN-62298). Weekly out-of-PR sweep → [SIN-62251](/SIN/issues/SIN-62251).

## Rollback

- Disable the verify gate by editing `deploy/scripts/stg-deploy.sh`, removing
  the `verify_image` call, and re-installing the script on the VPS via the
  documented `scp` + `install` flow in `docs/deploy/staging.md`. The change
  must go through PR review.
- Drop the cosign + SBOM steps from `cd-stg.yml` to revert to unsigned
  pushes; the registry still holds prior unsigned images, so older deploys
  remain reachable.
- Dependabot PRs are independent and can be paused per ecosystem with
  `enabled: false` in `.github/dependabot.yml`.
- The `security-alerts` gate can be made advisory-only by changing `exit 1`
  to `exit 0`; not recommended.

## Addendum — SIN-63281 (2026-05-22): GHCR namespace moved to the fork

After the 2026-05-13 fork↔upstream reconciliation (ADR 0085), CI runs in
`ia-dev-sindireceita/crm` and the previous hard-coded `pericles-luz/crm`
GHCR namespace in `cd-stg.yml` could no longer be authenticated: the
fork's `secrets.GITHUB_TOKEN` does not have `packages:write` on the
upstream owner's namespace. Result: 96/100 `cd-stg` runs between
2026-05-16 and 2026-05-22 failed at `build & push image` with
`unexpected status from HEAD request … 403 Forbidden`, and the CSP fix
from [SIN-63275](/SIN/issues/SIN-63275) never reached staging.

Resolution:

- `cd-stg.yml` and `build-backup-image.yml` now push to
  `ghcr.io/${{ github.repository_owner }}/crm` and
  `ghcr.io/${{ github.repository_owner }}/crm-backup`. The token always
  matches the namespace because both come from the same workflow run.
- `deploy/scripts/stg-deploy.sh` `EXPECTED_REPO` and
  `COSIGN_IDENTITY_REGEXP` both move to `ia-dev-sindireceita/crm`
  (literal — the on-host gate must NOT trust an arbitrary owner; the
  binding is part of the verify gate).
- A new lockstep invariant in `tools/supply-chain/test_workflow_invariants.sh`
  asserts the cd-stg workflow pushes under `github.repository_owner` AND
  the on-host script accepts the matching literal namespace. Drift in
  either direction breaks CI.

Operator steps after merge (one-time, per `docs/deploy/staging.md` §4):

1. `scp` the new `deploy/scripts/stg-deploy.sh` to
   `/opt/crm/stg/bin/deploy.sh` on the VPS.
2. Re-do `docker login ghcr.io` as `crm-deploy` with a PAT that has
   `read:packages` on the new namespace, OR mark the fork GHCR package
   public.

Until both steps are done, the next deploy will fail with either exit 65
(EXPECTED_REPO mismatch), exit 68 (cosign identity mismatch), or
`unauthorized` from `docker compose pull`.
