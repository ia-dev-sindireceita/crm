# ADR-0001: Dependency update policy (Dependabot triage)

- Status: Accepted
- Date: 2026-05-07
- Owner: CTO
- Related: SIN-62325

## Context

Dependabot opens PRs across three ecosystems (Go modules, Docker base images,
GitHub Actions). Auto-merging everything risks silent breaking changes
(major bumps, multi-major skips, supply-chain compromise of CI tooling).
Not updating risks unpatched CVEs and falling out of support.

## Decision

We triage Dependabot PRs by **risk tier**, not by source:

| Tier | What | Action |
|------|------|--------|
| 1. Patches & known-stable minors | Go patch/minor on direct deps; well-known libs (prometheus, goldmark) | CI green → merge |
| 2. Sensitive deps | Anything in the security perimeter (auth, crypto, sessions, HTTP routing); CI tooling (gitleaks, sbom, govulncheck); Go toolchain | Human review + smoke test |
| 3. 0.x bumps | Pre-1.0 libs with breaking minor bumps (templ, testcontainers) | Human review; explicit migration notes |
| 4. GitHub Actions majors | upload/download-artifact, checkout, setup-go, docker/* | Migration issue; do not merge raw Dependabot PR |
| 5. Multi-major skips | e.g. v3 → v8 in one PR | Always a migration issue; never merge raw |

### Specific rules

- **Go base-image major bumps** wait until `go.mod` toolchain is bumped in the
  same PR. We control the cadence; Dependabot PRs that bump only the image
  are closed.
- **`actions/upload-artifact` v3 ↔ v4+** are not interoperable. Upload and
  download must be migrated in the same PR.
- **No agent self-merge.** Agents prepare PRs; the human operator clicks
  merge. (See user-memory rule.)
- **Confidential security advisories** are not discussed on public issue
  threads. Escalate to CEO for a private channel.

## Consequences

- Slower update cadence on majors, traded for safer rollouts.
- Each major bump produces an explicit migration child issue — auditable.
- Dependabot config in `.github/dependabot.yml` enforces the tiering by
  ignoring majors on the deps listed above.

## Alternatives considered

- Auto-merge all green PRs: rejected — masks breaking 0.x and Action major
  changes (e.g. artifact v3↔v4 incompatibility).
- Disable Dependabot: rejected — drops CVE coverage.
- Renovate: deferred — Dependabot is sufficient at our scale.
