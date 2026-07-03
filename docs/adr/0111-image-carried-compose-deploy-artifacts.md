# ADR 0111 — Carry compose + caddy/unbound deploy artifacts inside the signed image and extract them with `docker cp`

- Status: Proposed (awaiting CTO + SecurityEngineer sign-off)
- Date: 2026-07-03
- Deciders: CTO, SecurityEngineer
- Drives: [SIN-66600](/SIN/issues/SIN-66600) (this ADR — design half of the drift-elimination guardrail)
- Motivated by: [SIN-66592](/SIN/issues/SIN-66592) (caddy `dns:["unbound"]` deploy crash where the fix was correct on `main` but the host was never re-synced) and its manual unblock [SIN-66599](/SIN/issues/SIN-66599)
- Builds on: [SIN-63332] image-carried `/migrations` + `docker cp` extraction in `deploy/scripts/stg-deploy.sh`, [ADR 0084](./0084-*.md)/SIN-62247 cosign keyless verify gate, [ADR 0110](./0110-backup-age-recipient-runtime-mount.md) (what stays host-mutable at runtime), [SIN-62332] compose↔unbound parity gate
- Lenses: **Reversibility**, **Least privilege / defense in depth**, **Boring technology**

## Context

Staging is deployed by `cd-stg`, which pushes a **cosign-signed** application
image to GHCR and then SSHes a forced-command wrapper (`stg-deploy.sh`, installed
as `/opt/crm/stg/bin/deploy.sh`) that runs `docker compose … up -d` against a
**host-resident** compose file at `/opt/crm/stg/compose.stg.yml`, plus caddy
configs under `/opt/crm/stg/caddy/` (`Caddyfile.stg`, `security-headers.caddy`,
`unbound.conf`).

Those on-host files are installed **once, by hand** (`scp` + `install`, see
`docs/deploy/staging.md` §5) and are never re-synced by CI. The CD pipeline only
pushes the image.

### Failure class (recurring drift)

Every time `deploy/compose/compose.stg.yml`, `deploy/caddy/*`, or
`infra/caddy/unbound.conf` changes on `main`:

1. CI stays green — the compose-unbound parity gate and every other check lint
   the **repo** copy.
2. `cd-stg` then runs `docker compose up` against the **stale host** copy.
3. The recreate blows up (or silently serves stale config), costing a full
   deploy cycle.

This is exactly what happened on SIN-66592: the `dns:["unbound"]` / static-IP
fix was correct on `main`, CI was green, but `/opt/crm/stg/compose.stg.yml` and
`/opt/crm/stg/caddy/unbound.conf` on the VPS were never updated, so CD stayed
red. SIN-66599 is the one-off manual `scp` unblock.

`/migrations` does **not** have this problem: SIN-63332 already ships it inside
the image and the `migrate-up` verb extracts it with `docker cp` from the
just-verified image. The compose + caddy artifacts are the last host-mutable
deploy inputs that still drift.

## Decision

Carry the deploy topology artifacts **inside the cosign-signed application
image** and have the deploy wrapper extract them with `docker cp` on every
deploy, exactly mirroring the `/migrations` pattern.

### Artifacts moved into the image

Add to the `crm-server` Dockerfile stage (read-only data layers, like
`/migrations`):

| Image path (proposed)              | Source in repo                         | On-host destination                       |
|------------------------------------|----------------------------------------|-------------------------------------------|
| `/deploy/compose.stg.yml`          | `deploy/compose/compose.stg.yml`       | `/opt/crm/stg/compose.stg.yml`            |
| `/deploy/caddy/Caddyfile.stg`      | `deploy/caddy/Caddyfile.stg`           | `/opt/crm/stg/caddy/Caddyfile.stg`        |
| `/deploy/caddy/security-headers.caddy` | `deploy/caddy/security-headers.caddy` | `/opt/crm/stg/caddy/security-headers.caddy` |
| `/deploy/caddy/unbound.conf`       | `infra/caddy/unbound.conf`             | `/opt/crm/stg/caddy/unbound.conf`         |

### Extraction flow (`deploy` verb, before `compose … up`)

1. `cosign verify` the image ref (existing gate at the top of `stg-deploy.sh`).
   No change — the extraction is downstream of the trust gate.
2. `carrier="$(docker create "${NEW_IMAGE}")"` (already pulled/cached by the
   deploy path; `docker create` does not run the entrypoint).
3. `docker cp "${carrier}:/deploy/." "${workdir}/"` into a `mktemp -d` staging
   dir; `docker rm -f "${carrier}"`.
4. **Fail closed** if any expected file is missing/empty inside the image
   (mirror the migrate-up guard, distinct non-zero exit code). Do **not** fall
   back to the stale host copy.
5. Atomically install the extracted files over `/opt/crm/stg/compose.stg.yml`
   and `/opt/crm/stg/caddy/*` (write-to-temp + `mv` within the same filesystem
   so a crash never leaves a half-written compose file).
6. Proceed to `docker compose --env-file .env.stg -f compose.stg.yml pull && up -d`
   using the **freshly extracted** compose file.

The on-host compose/caddy files become **derived, verified artifacts**, not
authoritative hand-edited inputs. Repo→host drift is structurally impossible:
the bytes CD runs are the bytes signed into the image, and the parity/lint gates
that already run on the repo copy now gate the deployed copy transitively (the
Dockerfile `COPY` makes repo == image).

## Trust-boundary implications (SecurityEngineer review focus)

### Now signed & tamper-evident (moved *into* the cosign boundary)

- `compose.stg.yml`, `Caddyfile.stg`, `security-headers.caddy`, `unbound.conf`
  are now part of the cosign-verified image. A host-side edit to
  `/opt/crm/stg/*` is **overwritten on the next deploy** — tampering is not
  durable, and the deployed topology provably matches a signed, CI-reviewed
  commit. This is a **net security improvement**: today those files are
  root-editable on the box with no signature and no drift detection.
- No new secrets enter the image. `compose.stg.yml` contains **zero secret
  literals** — every credential is `${VAR}`-interpolated from the host-only
  `.env.stg`, and the two MinIO refs are `$$`-escaped runtime container env, not
  build-time values (verified 2026-07-03). Caddy/unbound configs are likewise
  secret-free. The image gains **deploy topology** (ports, service names,
  volume names, tenant-host env-var *names*) — all already public in the repo.

### Still host-mutable at runtime (deliberately *outside* the image)

- `.env.stg` — all secrets (`POSTGRES_PASSWORD`, `MINIO_ROOT_*`, `DATABASE_URL`,
  tenant hosts). MUST stay off the image (same rule as ADR 0110's age recipient).
- The age backup public recipient runtime mount (ADR 0110) — unchanged.
- `deploy.sh` (the wrapper itself) — installed once via the forced-command SSH
  key; it is the *extractor*, so it cannot bootstrap itself from the image.

### Rollback story (improves on today)

- `.last-image` already records the previous `APP_IMAGE`. Because compose is now
  image-derived, rolling the image back **also** restores the compose/caddy
  files that match that image — today a rollback of the image against a
  new-shape host compose can leave an inconsistent stack. Rollback becomes
  "re-run `deploy <previous-image-ref>`" and the whole topology reverts atomically.
- Reversibility of the ADR itself: the change is additive (new Dockerfile COPY
  layers + a new extraction step). Reverting the wrapper to read the host copy
  restores today's behaviour with no data migration.

### First-install bootstrap (what the manual checklist shrinks to)

After this ships, `docs/deploy/staging.md` §5 manual `scp`+`install` reduces to:

- `deploy.sh` (the wrapper) → `/opt/crm/stg/bin/deploy.sh`
- an empty `.env.stg` (operator fills secrets)
- the directory skeleton (`/opt/crm/stg`, `/bin`, `/caddy`)

`compose.stg.yml` + all caddy/unbound files are **no longer** hand-installed —
the first `deploy` extracts them. (This also retires the SIN-66600 doc-fix leg
as a permanent concern.)

### Chicken-and-egg (one-time, unavoidable)

Shipping *this* wrapper still requires one final manual `deploy.sh` re-sync to
install the extraction-aware version — the old wrapper doesn't know how to
extract. That one-time install folds into SIN-66599 (or a dedicated sync
ticket). After that single sync, drift is gone permanently.

## Consequences

**Positive**

- Repo↔host compose/caddy/unbound drift is structurally eliminated.
- Deployed topology is cosign-signed and tamper-evident.
- Rollback reverts image + topology atomically.
- Manual first-install checklist shrinks; the SIN-66600 doc-fix class disappears.

**Negative / risks (for reviewer scrutiny)**

- The image now embeds deploy topology. Mitigation: no secrets, all already
  public in-repo; acceptable disclosure.
- Extraction adds a failure surface to the `deploy` verb. Mitigation: fail
  closed with a distinct exit code; never fall back to stale host files.
- Larger image by a few KB of text. Negligible.
- The forced-command wrapper gains a `docker create`/`docker cp`/`mv` step on
  the deploy path — audited, no new capabilities beyond what `migrate-up`
  already uses.

## Alternatives considered

1. **Automate `scp` from CI (rsync the repo copy to the host).** Rejected:
   re-introduces an SSH write path with broad filesystem reach outside the
   cosign trust boundary, and the pushed bytes are *not* signature-bound to the
   deployed image.
2. **CI gate that diffs repo vs host over SSH and fails on drift.** Detects but
   does not fix drift; still needs a manual re-sync and a second SSH surface.
3. **Status quo + discipline.** Rejected — this is the third+ drift incident;
   process discipline has already failed empirically (SIN-66592).

## Follow-up implementation tickets (created only after sign-off)

1. Dockerfile: add the `/deploy` COPY layers.
2. `stg-deploy.sh`: add the extraction+atomic-install step to the `deploy` verb,
   with fail-closed guards and tests (hermetic wrapper test in the SIN-65902
   style — sandbox paths, shimmed `docker`).
3. `docs/deploy/staging.md` §5: rewrite the manual install to the shrunk
   bootstrap; note the one-time re-sync.
4. Optional CI check asserting the image `/deploy/*` bytes equal the repo bytes
   (defence in depth against a Dockerfile COPY drifting from source paths).

The dual gate (CTO + SecurityEngineer) applies to every implementation PR
because they touch the deploy trust boundary.
