# 0102. Backup pipeline runs as a compose sidecar, not a host-installed systemd unit

Date: 2026-05-21

## Status

Accepted.

## Context

The original encrypted backup pipeline ([SIN-62250](/SIN/issues/SIN-62250) / [SIN-62267](/SIN/issues/SIN-62267), legacy PRs #51 and #54)
landed as a host-installed `sindireceita-backup.service` + `.timer` pair. Those commits
were lost in the 2026-05-09 fork reset ([ADR-0085](0085-fork-upstream-reconcile.md)) and are preserved only on tag
`main-legacy-pre-reconcile-20260509`.

Between the original landing and this re-land, Fase 0 PR9 ([SIN-62215](/SIN/issues/SIN-62215)) shifted the
deploy contract to a containerized stack: `deploy/compose/compose.stg.yml` is the
single source of truth for stg, CD (`.github/workflows/cd-stg.yml`) writes digests
into `/opt/crm/stg/.env.stg` and runs `docker compose pull && up`. Prod
(`compose.yml`) follows the same shape. Host-installed systemd units fall outside
that CD pipeline.

## Decision

Backup runs as a sidecar service in `compose.stg.yml` and `compose.yml`. Scheduling
is handled by supercronic inside the container; logs go to stdout (Loki); the age
public key ships in the image; the age private key is only mounted at restore-pipeline
invocation, never into the scheduled service.

Image build + publish lives in a **separate, path-gated workflow**
(`.github/workflows/build-backup-image.yml`) that fires only when
`infra/backup/**` or `scripts/backup*.sh` change. The compose service
consumes the image via the env-var pattern `${BACKUP_IMAGE:?required}`,
identical in shape to `${APP_IMAGE:?required}` (introduced in Fase 0 PR9
/ [SIN-62215](/SIN/issues/SIN-62215)). The operator bootstraps and bumps
`BACKUP_IMAGE=` in `/opt/crm/stg/.env.stg` via the existing
`docs/deploy/staging.md` → "Bumping infra image digests" procedure
(CTO-confirmed on the SIN-63195 thread, 2026-05-21: "do not refactor
back to extending `cd-stg.yml` + `stg-deploy.sh`"; see "Alternatives
considered" #4 below for the trade-off summary). This deliberately
decouples the backup-image cadence from the app deploy pipeline:
- `cd-stg.yml` stays single-purpose (resolve digest → SSH → compose pull → up).
- `build-backup-image.yml` rebuilds + cosign-signs the backup image
  only when its inputs change.
- Reversible: the backup workflow can be reverted independently without
  touching the app's build/deploy pipeline.

## Consequences

Positive:

- Backup is in CD. A `git revert` of the backup PR + `docker compose up` is a full
  rollback.
- No out-of-band VPS state to install/uninstall when the deploy shape changes.
- Hardening invariants (non-root, read-only FS, tmpfs /tmp, cap_drop ALL, no new
  privileges, mem/cpu limits) are preserved at the container boundary.
- Logs flow through the same promtail → Loki path as every other service.

Negative:

- We lose journald-native structured fields. Mitigation: emit key=value to stdout;
  promtail and Loki parse those fields natively.
- supercronic adds one binary to maintain (vs. systemd-timer in-base). Pinned by
  SHA256 of the published release binary; updates tracked alongside other infra
  images.
- The backup image is one more artifact CI has to build and push. Mitigation:
  the build is path-gated (`.github/workflows/build-backup-image.yml` only fires
  when `infra/backup/**` or `scripts/backup.sh` change), so the steady state cost
  per app deploy is zero.

## Hardening invariant mapping

The legacy systemd unit (`infra/systemd/sindireceita-backup.service` in tag
`main-legacy-pre-reconcile-20260509`) used these primitives. The container
sidecar reproduces each one at the docker/compose layer:

| Legacy systemd primitive                                | Compose sidecar equivalent                                                                 |
| ------------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| `User=sindireceita-backup`                              | `user: "65534:65534"` (nobody) inside container                                            |
| `NoNewPrivileges=true`                                  | `security_opt: ["no-new-privileges:true"]`                                                 |
| `PrivateTmp=true`                                       | `tmpfs: [/tmp]` (dump + ciphertext staging)                                                |
| `ProtectSystem=strict`                                  | `read_only: true` on root FS                                                               |
| `ProtectHome=true`                                      | n/a — no `/home` in image                                                                  |
| `ProtectKernelTunables` + `ProtectKernelModules`        | container default (rootless + no `--privileged`)                                           |
| `RestrictSUIDSGID=true`                                 | `cap_drop: [ALL]` removes setuid/setgid effective caps                                     |
| `MemoryDenyWriteExecute=true`                           | n/a at compose level — enforced by Go binary, age, pg_dump being W^X                       |
| `SystemCallFilter=@system-service`                      | docker default seccomp profile is strict; no override                                      |
| `ReadOnlyPaths=/opt/sindireceita /etc/sindireceita`     | image is read-only by construction                                                         |
| `InaccessiblePaths=…/age-backup.key`                    | private key NEVER bind-mounted into the sidecar; only into the on-demand restore-drill invocation |
| `EnvironmentFile=/etc/sindireceita/backup.env` (0640)   | `env_file: /opt/crm/stg/.env.stg` (already 0600 root:docker on the VPS per existing convention) |
| `MemoryMax=2G`                                          | `mem_limit: 2g`                                                                            |
| `CPUQuota=200%`                                         | `cpus: "2.0"`                                                                              |
| `TimeoutStartSec=2h`                                    | supercronic wallclock guard + script-level (script exits after pipeline; no infinite hang) |
| journald structured logs (`logger -t sindireceita-backup`) | structured key=value to stderr, captured by promtail/Loki                              |

The "private key never reaches the sidecar" invariant is enforced by
`internal/backup.TestComposeBackupSidecarDeniesPrivateKey`. The compose-level
hardening invariants above are each grep-asserted by
`internal/backup.TestComposeBackupSidecarHardeningInvariants`.

## Scheduling

Inside the sidecar: **supercronic** (purpose-built cron for containers,
signal-aware, no PID-1 reaping issues). Pinned by URL + SHA256 in
`infra/backup/Dockerfile` (boring-tech budget: not in Alpine's main/community
repo, so we accept the download-and-verify dance instead of pulling from
`--repository edge` or building from source).

Cron expression: `15 3 * * *` America/Sao_Paulo (03:15 BRT daily) — matches the
legacy `OnCalendar=*-*-* 03:15:00 America/Sao_Paulo` from the legacy `.timer`
file.

Manual one-shot invocation: `docker compose run --rm backup
/usr/local/bin/backup.sh` (for stg/prod smoke tests).

## Restore pipeline stays out-of-band

The encrypted-restore pipeline (`scripts/backup-restore.sh` →
`/usr/local/bin/backup-restore.sh` in the image) is **not** part of the
scheduled sidecar service. It is invoked manually:

```bash
docker compose run --rm \
  --user 0:0 \
  -v /etc/sindireceita/age-backup.key:/etc/sindireceita/age-backup.key:ro \
  --env BACKUP_AGE_KEY=/etc/sindireceita/age-backup.key \
  --env PGHOST=postgres --env PGDATABASE=sindireceita_drill \
  --env PGUSER=drill --env PGPASSWORD="$DRILL_PASSWORD" \
  backup /usr/local/bin/backup-restore.sh
```

Three invariants here, asserted by tests:

1. `--user 0:0` is required because the bind-mounted host key is
   `0440 root:sindireceita-backup`; the container's default UID (65534
   / nobody) is not in that group. Running the restore container as root
   inside the namespace reads the file directly. The container still has
   `read_only: true`, `cap_drop: ALL`, `no-new-privileges:true`, and
   `tmpfs: /tmp` from the compose service definition — only the UID
   inside the container changes vs the scheduled cron run.
2. `PG*` env vars (NOT `--dbname=postgres://user:pw@host/db`) keep the
   restore-target password out of `ps aux`. SIN-63195 SE-review MEDIUM #1.
3. The private age key is bind-mounted only at restore-pipeline
   invocation; the scheduled backup service has zero filesystem path to
   reach it. This preserves the legacy `InaccessiblePaths` invariant.

A separate, higher-level quarterly drill orchestrator
(`scripts/restore-drill.sh`, introduced by SIN-63187 / PR #226)
provisions an isolated docker stack, calls the inner pipeline above for
the real (non-synthetic) decrypt path, validates `/health` and a sample
DB query, then writes a dated drill report. The two scripts compose;
they are not duplicates.

Defense-in-depth lens: even if the scheduled container is compromised,
the private key cannot decrypt the backups (the recipient half is
public-only).

## Alternatives considered

1. **Re-apply host systemd unit unchanged.** Rejected: introduces a manual host
   install step that CD does not own and no rollback path tied to a git revert.
   Also re-introduces the `SystemCallFilter=~@privileged @resources` +
   `MemoryDenyWriteExecute=true` regression that motivated [SIN-62260](/SIN/issues/SIN-62260)
   (silent kill of pg_dump on AWS CLI v2 PyOxidizer + `setrlimit` paths).

2. **Run pg_dump from the app container via `docker exec`.** Rejected: violates
   least privilege (the app would need pg_dump + age + aws-cli baked in) and
   couples backup lifecycle to app lifecycle. Also makes "run backup ad-hoc
   without disturbing the running app" impossible.

3. **External backup service (Restic, Backblaze native).** Rejected for now:
   more moving parts than needed for current data volume; revisit when DB
   exceeds the pg_dump time budget. Current pg_dump → age → S3 cp pipeline
   fits inside the wall-clock budget for the size of data we have.

4. **CD writes BACKUP_IMAGE into .env.stg on every app deploy.** Rejected: this
   was the literal AC in the SIN-63195 plan, but it would re-push an unchanged
   backup image on every app CD run (wasteful + confusing audit trail) and
   would require extending the `stg-deploy.sh` SSH-constrained signature
   (single-arg → two-arg) in a coordinated VPS-side update. The current model
   (path-gated `.github/workflows/build-backup-image.yml` + operator-pasted
   `BACKUP_IMAGE=` line in `.env.stg`) decouples the cadences cleanly and
   matches the existing "Bumping infra image digests" runbook.

## References

- Legacy commits: `97e0e918bc9085c3599ff23338f36fa043f7f1c9` ([SIN-62250](/SIN/issues/SIN-62250), PR #51),
  `11658dbff94529e2c0fc525c9496b9d5624efcda` ([SIN-62267](/SIN/issues/SIN-62267), PR #54).
- Legacy tag: `main-legacy-pre-reconcile-20260509`.
- [ADR-0085](0085-fork-upstream-reconcile.md) (fork reset).
- [ADR-0086](0086-fork-only-migration-numbering.md) (migration renumbering).
- Re-landing batches 0–19 ([SIN-62510](/SIN/issues/SIN-62510)..[SIN-62529](/SIN/issues/SIN-62529)).
- [SIN-62199](/SIN/issues/SIN-62199) (Fase 6 parent — LGPD + restore drill gate).
- [SIN-62260](/SIN/issues/SIN-62260) (sandbox smoke test — superseded by this ADR; closed `cancelled` on this PR's merge).
