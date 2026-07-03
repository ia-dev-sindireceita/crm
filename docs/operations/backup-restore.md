# Backup & restore (encrypted Postgres dumps)

LMHost Postgres backups are encrypted client-side with [age](https://age-encryption.org)
before they leave the staging/prod compose stack. Even if Backblaze B2 / S3 credentials leak,
an attacker who downloads every snapshot still sees nothing but ciphertext.

This runbook covers daily operation, key rotation, and the catastrophic loss
scenarios. Lands in Fase 6 ([SIN-62199](/SIN/issues/SIN-62199)) as part of
[SIN-62250](/SIN/issues/SIN-62250); re-landed against the container stack
in [SIN-63195](/SIN/issues/SIN-63195) (see [ADR-0102](../adr/0102-backup-compose-sidecar.md)).

> **Architecture note.** The pipeline runs as the `backup` compose service
> in `deploy/compose/compose.stg.yml` (staging) and `deploy/compose/compose.yml`
> (local dev). Scheduling is handled by supercronic inside the container; the
> host has no `lmhost-backup.service` / `.timer` to install or maintain.
> See [ADR-0102](../adr/0102-backup-compose-sidecar.md) for the decision and
> the 1:1 hardening-invariant mapping vs the legacy host systemd unit.

## Files

| Path | Role |
|------|------|
| `infra/age-backup.pub` | Public recipient **placeholder**, committed and baked into the image as the fallback default. CI pins it to the placeholder (`TestPublicRecipientParses`). The real recipient is injected at runtime (SIN-66537) — see the host path below. |
| `/etc/lmhost/age-backup.pub` | Real public recipient on the **staging/prod host**. Mode `0640`, owner `root:lmhost-backup`. Bind-mounted read-only into the scheduled `backup` service and pointed to by `BACKUP_AGE_RECIPIENTS`; overrides the baked placeholder. Public key material only — never the private key. NEVER committed. See [ADR-0110](../adr/0110-backup-age-recipient-runtime-mount.md). |
| `infra/sops/age-backup.key.enc` | SOPS-encrypted private key. Committed (ciphertext). |
| `infra/backup/Dockerfile` | Sidecar image: Alpine + postgres-client + age + aws-cli + supercronic. |
| `infra/backup/crontab` | supercronic crontab — fires `/usr/local/bin/backup.sh` at 03:15 America/Sao_Paulo. |
| `/etc/lmhost/age-backup.key` | Decrypted private key on the **staging/prod host**. Mode `0440`, owner `root:lmhost-backup` (set by `scripts/generate-backup-key.sh`). NEVER committed. NEVER bind-mounted into the scheduled sidecar — only into manual `backup-restore.sh` invocations that pass `--user 0:0` so the container-side UID matches the file's `0440 root:*` ownership. |
| `/opt/crm/stg/.env.stg` | Compose env-file. Holds `BACKUP_IMAGE`, `BACKUP_BUCKET`, `AWS_*`, `DATABASE_URL`, `BACKUP_NODE_ID`. Mode `0600`, owner `crm-deploy:crm-deploy`. NEVER committed. |
| `lmhost-backup-state` (named docker volume) | Holds `backup-last-success.json` so the size-threshold check has prior-day data to compare against. |
| `scripts/backup.sh` | `pg_dump` → `age -R` → `aws s3 cp` with per-stage exit checks, dump-size threshold, S3 head-object verify, structured stderr logs. Baked into the sidecar image at `/usr/local/bin/backup.sh`. |
| `scripts/backup-restore.sh` | `aws s3 cp - \| age -d -i KEY \| pg_restore`. Inner restore pipeline. Baked into the sidecar image at `/usr/local/bin/backup-restore.sh`; the private age key is bind-mounted ad-hoc at invocation time AND the invocation passes `--user 0:0` so UID inside container matches the host-side `0440 root:lmhost-backup` ownership. Uses libpq `PG*` env vars (NOT a URL on argv) so the restore target's password is invisible to `ps aux`. |
| `scripts/restore-drill.sh` | Outer quarterly drill orchestrator (SIN-63187, PR #226). Provisions an isolated docker stack, calls into the inner restore pipeline above for the real (non-synthetic) decrypt path, validates `/health` + DB queries, writes a dated drill report. |
| `scripts/generate-backup-key.sh` | Bootstraps the keypair. **Runs on the host, not in the container** — the private key never enters the image. |
| `scripts/tests/backup_test.sh` | Hermetic shell-test harness for `backup.sh` (run with `bash scripts/tests/backup_test.sh`). |
| `internal/backup/*.go` | Go-level invariant tests: placeholder integrity, ciphertext shape, compose-service security posture. |

## Primeira instalação (first-time setup)

Run on a fresh staging/prod host before turning the compose stack on. Each
step assumes a shell with `sudo` on the host.

1. **Provision the compose stack** per `docs/deploy/staging.md` (Fase 0 PR9 /
   [SIN-62215](/SIN/issues/SIN-62215)). The `crm-deploy` user, `/opt/crm/stg`
   directory, and SSH-constrained deploy key are prerequisites for everything
   below; this runbook layers on top of that base install.
2. **Create the `lmhost-backup` group and lay down `/etc/lmhost`**.
   `scripts/generate-backup-key.sh` writes the private key as
   `0440 root:lmhost-backup`; the group MUST exist before that script
   runs (the script aborts if it does not).
   ```bash
   sudo groupadd --system lmhost-backup
   sudo install -d -m 0750 -o root -g lmhost-backup /etc/lmhost
   ```
   Owner `root`, group `lmhost-backup` so a future audit can grant a
   second human key custodian read access by adding them to that group
   without `sudo`. The container-side restore path uses `--user 0:0`
   instead of group membership (see § "Restore drill") — root inside the
   container reads the host-side `0440` key directly. The `crm-deploy`
   user does not need the key (and is not in the group) — it only invokes
   `docker compose run --rm --user 0:0 …` which gets uid 0 inside the
   container.
3. **Provision compose-side env in `/opt/crm/stg/.env.stg`.** The compose
   `backup` service reads this file via `env_file`. Required additions
   beyond the base set provisioned by `staging.md`:
   ```ini
   # Bootstrapped from `.github/workflows/build-backup-image.yml`'s job
   # summary after the first push to `infra/backup/**`. Same model as
   # APP_IMAGE — see docs/deploy/staging.md § "Bumping infra image digests".
   BACKUP_IMAGE=ghcr.io/pericles-luz/crm-backup@sha256:<digest from job summary>

   # Object-store credentials and bucket. Re-used by aws-cli inside the
   # sidecar; never echoed to logs.
   BACKUP_BUCKET=lmhost-backups
   AWS_ACCESS_KEY_ID=...
   AWS_SECRET_ACCESS_KEY=...
   # optional, for Backblaze B2 / non-AWS endpoints:
   AWS_ENDPOINT_URL=https://s3.us-west-002.backblazeb2.com

   # Optional per-host identifier. Defaults to "stg" inside the compose service.
   # BACKUP_NODE_ID=stg-vps-01

   # Optional TZ override. Default America/Sao_Paulo matches the legacy
   # systemd unit's OnCalendar timing (cron line `15 3 * * *`).
   # BACKUP_TZ=America/Sao_Paulo

   # SIN-66566 — point the sidecar at the DEDICATED read-only backup role
   # (app_backup: BYPASSRLS + pg_read_all_data, migration 0133) so pg_dump can
   # read the entire RLS-guarded schema. The app role (app_runtime, NOBYPASSRLS)
   # CANNOT dump the 191-RLS-policy schema and fails permission-denied. The
   # password is the one you set with `\password app_backup` (DB-role step
   # below). If BACKUP_DATABASE_URL is unset, the sidecar falls back to the
   # app-role DSN — a degraded default that only works where RLS is absent.
   BACKUP_DATABASE_URL=postgres://app_backup:<password>@postgres:5432/crm?sslmode=disable

   # SIN-66566 — staging's COMPLETE dump is ~682 KB, BELOW the 1 MiB bootstrap
   # anti-truncation floor, so a fresh install aborts every backup with
   # `stage=pg_dump status=fail reason=size-below-threshold bytes=698199
   # min=1048576`. Lower the floor for staging; production (a much larger DB)
   # should leave it UNSET to keep the safe 1 MiB default. Must be a positive
   # integer count of bytes; a non-integer aborts at preflight.
   BACKUP_MIN_BYTES_FLOOR=262144   # 256 KiB
   ```

   **DB role (`app_backup`).** Apply migration `0133_app_backup_role.up.sql`
   as part of the normal migration run (it needs superuser, like
   `0001_roles.up.sql` — CREATE ROLE is cluster-scoped). The migration creates
   `app_backup` with `LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS` and
   grants `pg_read_all_data`; it deliberately does NOT set a password (same
   model as `app_runtime`/`app_admin`). Set one out-of-band, then use it in
   `BACKUP_DATABASE_URL` above:
   ```bash
   # As a superuser against the crm database:
   sudo -u postgres psql -d crm -c "\password app_backup"
   # Verify the role shape (must be rolbypassrls=t, rolsuper=f):
   sudo -u postgres psql -d crm -tAc \
     "SELECT rolname, rolsuper, rolbypassrls, rolcanlogin FROM pg_roles WHERE rolname='app_backup'"
   ```
   `app_backup` can read every table and ignore RLS, but has no write, DDL, or
   superuser rights — the least privilege pg_dump needs.
4. **Generate the recipient keypair** (also used during rotation; see below).
   The script runs on the host (not in the container) so the private key
   never enters the image filesystem:
   ```bash
   sudo ./scripts/generate-backup-key.sh
   ```
   The script refuses to overwrite an existing key and writes the result to
   `/etc/lmhost/age-backup.key` `0440 root:lmhost-backup`. The
   script also prints the matching public recipient on stdout — capture it
   for the next step.

   > **Group note.** The legacy host-systemd unit ran as a dedicated
   > `lmhost-backup` Unix group so the unit could read the private
   > key only via group membership. In the compose model the scheduled
   > sidecar does **not** read the private key at all — only ad-hoc
   > `restore-drill` invocations do, via an explicit `-v` bind mount.
   > The `lmhost-backup` group on the host is now only used by
   > `generate-backup-key.sh`'s chown step; if you do not have a Unix
   > policy reason to keep it, you may collapse the group to `root` and
   > pass the path explicitly to `docker compose run -v
   > /etc/lmhost/age-backup.key:...:ro`.

5. **Write the real recipient to the host at `/etc/lmhost/age-backup.pub`
   ([SIN-66537](/SIN/issues/SIN-66537)).** The recipient printed by
   `scripts/generate-backup-key.sh` in step 4 is injected into the sidecar
   **at runtime** via a read-only host mount — it is *not* baked into the
   signed image. Use the pinned placeholder image as-is; **no image rebuild
   or re-bake is required.**
   ```bash
   # Paste the recipient line printed by generate-backup-key.sh in step 4:
   printf '%s\n' 'age1...<your real recipient>' \
     | sudo install -m 0640 -o root -g lmhost-backup /dev/stdin \
       /etc/lmhost/age-backup.pub
   ```
   The `backup` service in `compose.stg.yml` bind-mounts this file read-only
   at the same in-container path and points `BACKUP_AGE_RECIPIENTS` at it, so
   the mount overrides the placeholder that ships inside the image
   (`infra/age-backup.pub`, kept as a non-functional fallback default). File
   mode `0640 root:lmhost-backup` places the recipient in the same
   `/etc/lmhost` trust boundary as the private key (created in step 2), but it
   is only *public* key material — the scheduled sidecar never reads the
   private key.

   Rationale (see [ADR-0110](../adr/0110-backup-age-recipient-runtime-mount.md)):
   the committed `infra/age-backup.pub` stays the placeholder forever — CI
   asserts it (`TestPublicRecipientParses` in `internal/backup`) and only
   upstream `pericles-luz/crm` may push the cosign-signed image to GHCR, so
   the real recipient cannot reach the image anyway. Injecting it at runtime
   dissolves that gap with zero changes to the build/sign pipeline, and makes
   recipient rotation an operator file swap (see § "Rotação de chave") rather
   than an image rebuild.

   If `/etc/lmhost/age-backup.pub` is missing when the scheduled sidecar runs
   with the profile activated, the container start fails on the missing bind
   source (config render still succeeds — the file only has to exist at run
   time), so a forgotten recipient fails loud rather than silently encrypting
   to the non-functional placeholder.

   > **Restore path unchanged.** This step covers only the *public* recipient.
   > The *private* key stays at `/etc/lmhost/age-backup.key`
   > (`0440 root:lmhost-backup`, step 4) and is bind-mounted read-only **only**
   > for the out-of-band restore drill via `--user 0:0` — see § "Restore drill
   > (Fase 6)". The scheduled sidecar never mounts it.

6. **SOPS-encrypt the private key** so a fresh host can be re-bootstrapped
   from git without out-of-band copying:
   ```bash
   sudo sops --encrypt --age "$SOPS_AGE_RECIPIENT" \
        /etc/lmhost/age-backup.key \
        > infra/sops/age-backup.key.enc
   ```
   See `infra/sops/README.md` for `SOPS_AGE_RECIPIENT` and the
   recipient-distinctness rule.
7. **Stash a second copy of the cleartext private key in the offline cofre**
   — see § "Cofre offline" for the storage policy and the verification cadence.
8. **Verify the install** with a dry-run against the running compose stack.
   The image `ENTRYPOINT` is `supercronic` (it treats its argument as a
   crontab path), so a one-shot MUST override it with `--entrypoint` to run
   the script directly — without it, supercronic tries to parse `backup.sh`
   as a crontab and aborts with `bad crontab line: 'set -Eeuo pipefail'`:
   ```bash
   sudo -u crm-deploy docker compose -f /opt/crm/stg/compose.stg.yml \
     --env-file /opt/crm/stg/.env.stg \
     run --rm --entrypoint /usr/local/bin/backup.sh backup
   sudo -u crm-deploy docker compose -f /opt/crm/stg/compose.stg.yml logs --tail=200 backup
   ```
   A clean run ends with a `stage=done status=ok bytes=… dump_bytes=… target=s3://…`
   line (level `info`) and the container exits 0. The first run logs
   `bootstrap=true` and `min_bytes=1048576` because no prior state file
   exists; subsequent runs raise `min_bytes` to 10% of the last successful
   `dump_bytes` (see § "Dump size threshold and state file").

   The one-shot `docker compose run --rm backup …` above works even before the
   next step because `run` auto-enables the service's own profile; the
   *scheduled* sidecar, however, only starts once you activate the profile in
   step 9.
9. **Activate the scheduled sidecar (opt-in profile,
   [SIN-66535](/SIN/issues/SIN-66535)).** The `backup` service is guarded by
   `profiles: ["backup"]`, so a plain `docker compose up -d` deliberately
   **skips** it — a host where backups have never been provisioned (no key, no
   `BACKUP_IMAGE`) can still bring the rest of the stack up. The image line uses
   the soft `${BACKUP_IMAGE:-…}` default (not the mandatory `:?` form) because
   Compose interpolates the whole file before applying profiles, so `:?` would
   otherwise still abort the default `up`. Once steps 1–8 are complete, opt the
   daily sidecar in explicitly:
   ```bash
   sudo -u crm-deploy docker compose -f /opt/crm/stg/compose.stg.yml \
     --env-file /opt/crm/stg/.env.stg \
     --profile backup up -d backup
   # Equivalent: export COMPOSE_PROFILES=backup before `docker compose up -d`.
   ```
   The container then runs supercronic and fires `backup.sh` on the 03:15
   America/Sao_Paulo schedule. Until this step runs, the scheduled backup does
   **not** exist on the host. If you activate `--profile backup` **before**
   bootstrapping `BACKUP_IMAGE` in `.env.stg`, the pull fails loud with a
   `manifest unknown` error on the placeholder tag
   (`crm-backup:SET-BACKUP_IMAGE-IN-ENV-STG`) — set the real digest (step 3)
   first.

## Daily operation

> **Opt-in profile ([SIN-66535](/SIN/issues/SIN-66535)).** The scheduled
> `backup` sidecar only runs when the `backup` compose profile is active (see
> first-time-setup step 9). A default `docker compose up -d` does not start it;
> use `docker compose --profile backup up -d` (or `COMPOSE_PROFILES=backup`).
> The one-shot `docker compose run --rm backup …` commands below auto-enable
> the profile for their single invocation, so they work regardless.

The supercronic crontab inside the `backup` sidecar fires at 03:15
America/Sao_Paulo every day (cron line `15 3 * * *` in
`infra/backup/crontab`); the schedule matches the legacy
`OnCalendar=*-*-* 03:15:00 America/Sao_Paulo` timing from the systemd unit.
Logs go to docker → promtail → Loki via the host's default log driver.

```bash
# Inspect last 24h of structured logs (use Loki/Grafana for steady-state
# monitoring; the docker compose path below is the in-VPS debug fallback).
sudo -u crm-deploy docker compose -f /opt/crm/stg/compose.stg.yml \
  --env-file /opt/crm/stg/.env.stg \
  logs --since 24h backup

# Trigger an out-of-cycle backup (manual one-shot — does NOT touch the cron
# schedule, fires backup.sh exactly once and exits). `--entrypoint` overrides
# the image's supercronic ENTRYPOINT so backup.sh runs directly instead of
# being parsed as a crontab.
sudo -u crm-deploy docker compose -f /opt/crm/stg/compose.stg.yml \
  --env-file /opt/crm/stg/.env.stg \
  run --rm --entrypoint /usr/local/bin/backup.sh backup
```

Smoke-check that an object actually landed. Objects are nested under the
node id (`BACKUP_NODE_ID` from `.env.stg`, defaults to "stg"):

```bash
NODE_ID=${BACKUP_NODE_ID:-stg}
aws s3 ls "s3://$BACKUP_BUCKET/$(date -u +%F)/$NODE_ID/"
# Expect: dump.pgc.age <some-mb>
```

Sanity-check that the object is real age ciphertext (not, e.g., a captured
error page that just happens to be 200 OK):

```bash
aws s3 cp "s3://$BACKUP_BUCKET/$(date -u +%F)/$NODE_ID/dump.pgc.age" - \
  | head -c 32 | xxd
# Expect first line to start with: 6167652d 656e6372 79707469 6f6e2e6f
# (= "age-encryption.o" — age v1 magic).
```

### Structured logs ([SIN-62267](/SIN/issues/SIN-62267) → [SIN-63195](/SIN/issues/SIN-63195))

Every stage of `backup.sh` emits a `key=value` record on stderr. Docker
captures stderr verbatim; promtail scrapes the container log stream and
Loki indexes the records under the `service=lmhost-backup` label.
Filter by stage to debug a slow or failing run:

```logql
# Loki / Grafana query
{compose_service="backup"} | logfmt
  | service="lmhost-backup"
  | line_format "{{.ts}} {{.level}} {{.stage}} {{.status}}"
```

Record shape (one line per record):

```text
ts=2026-05-21T03:15:01Z level=info  service=lmhost-backup stage=start    bootstrap=… min_bytes=… last_bytes=… target=s3://…
ts=2026-05-21T03:15:02Z level=info  service=lmhost-backup stage=pg_dump  status=ok dur_ms=… bytes=…
ts=2026-05-21T03:15:03Z level=info  service=lmhost-backup stage=encrypt  status=ok dur_ms=… bytes=…
ts=2026-05-21T03:15:08Z level=info  service=lmhost-backup stage=upload   status=ok dur_ms=… bytes=…
ts=2026-05-21T03:15:09Z level=info  service=lmhost-backup stage=verify   status=ok dur_ms=… bytes=…
ts=2026-05-21T03:15:09Z level=info  service=lmhost-backup stage=done     status=ok bytes=… dump_bytes=… target=s3://…
```

Failures land at `level=err` with a `reason=…` field; alert on
`level=err` filtered to `service=lmhost-backup`. Secrets
(`DATABASE_URL`, `AWS_*`, dump content) are intentionally never echoed —
the log surface is metadata only, enforced by
`scripts/tests/backup_test.sh::test_database_url_not_logged` and
`test_no_dump_bytes_in_logs`.

### Dump size threshold and state file ([SIN-62267](/SIN/issues/SIN-62267))

`backup.sh` size-checks the cleartext `pg_dump` output before encrypting
to catch a silent truncation (e.g. seccomp killing a syscall the dump
relied on, or `pg_dump` exiting 0 after an in-progress crash). The
threshold is dynamic:

- **Bootstrap (no state file yet):** `min_bytes = 1 MiB`. Logged as
  `bootstrap=true`.
- **Steady state:** `min_bytes = max(1 MiB, 10% of last successful
  dump_bytes)`. The script reads `dump_bytes` from
  `/var/lib/lmhost/backup-last-success.json` (the
  `lmhost-backup-state` named docker volume; defined in
  `deploy/compose/compose.stg.yml`).

The state file is written via atomic rename only after every stage —
including the post-upload `aws s3api head-object` verify — succeeds. A
failure anywhere in the chain leaves the previous state file untouched,
so the threshold derived from the last good run keeps protecting the
next attempt.

If you intentionally want to reset the baseline (e.g. after a planned
schema purge that legitimately shrinks the dump), wipe the named volume
on the backup host:

```bash
sudo -u crm-deploy docker volume rm crm-stg_lmhost-backup-state
# The volume is recreated empty on the next compose up; the next run logs
# bootstrap=true again.
sudo -u crm-deploy docker compose -f /opt/crm/stg/compose.stg.yml \
  --env-file /opt/crm/stg/.env.stg up -d backup
```

## Restore drill (Fase 6)

The restore drill must run end-to-end at least once per quarter. It re-hydrates
the latest dump into an ephemeral Postgres and runs a smoke query. The
restore-pipeline invocation is the **only** path that mounts the private
age key into a container — the scheduled backup service never sees it.

The quarterly drill *orchestrator* (`scripts/restore-drill.sh`, SIN-63187 /
PR #226) provisions an isolated drill stack and writes a dated report. The
*inner* restore pipeline below (`backup-restore.sh`) is what the
orchestrator calls for the real (non-synthetic) decrypt path, and what an
operator runs ad-hoc during incident response.

```bash
# Run as the crm-deploy user (member of docker) so docker compose run
# succeeds without sudo. The private key is bind-mounted read-only.
#
# --user 0:0 is REQUIRED. The sidecar image runs as nobody (UID 65534)
# by default; the host-side key is `0440 root:lmhost-backup`, and
# bind mounts preserve host UID/GID — nobody cannot read it. Running the
# restore container as root inside the namespace lets it read the
# bind-mounted key. The container still has `read_only: true`, `cap_drop:
# ALL`, `no-new-privileges:true`, and `tmpfs: /tmp` from the compose
# service definition; the only attack-surface change vs the scheduled
# cron run is the UID inside the container, which has no host effect
# (uid 0 inside a non-userns-mapped container is uid 0 on the host but
# bounded by cap_drop ALL and read-only root FS). See SIN-63195
# SE-review BLOCKER #2 for the rationale.
#
# PG* env vars (NOT a URL on argv) keep the restore-target password out
# of `ps aux`. SIN-63195 SE-review MEDIUM #1.
sudo -u crm-deploy docker compose -f /opt/crm/stg/compose.stg.yml \
  --env-file /opt/crm/stg/.env.stg \
  run --rm \
    --user 0:0 \
    --entrypoint /usr/local/bin/backup-restore.sh \
    -v /etc/lmhost/age-backup.key:/etc/lmhost/age-backup.key:ro \
    -e BACKUP_AGE_KEY=/etc/lmhost/age-backup.key \
    -e PGHOST=postgres \
    -e PGPORT=5432 \
    -e PGDATABASE=lmhost_drill \
    -e PGUSER=drill \
    -e PGPASSWORD="$DRILL_PASSWORD" \
    -e RESTORE_VERIFY_SQL='select count(*) from users' \
    -e RESTORE_VERIFY_MIN=1 \
    backup
```

Pass criteria:

- `pg_restore --exit-on-error` finishes 0.
- Smoke query returns >= `RESTORE_VERIFY_MIN`.
- Cleartext dump never lands on the host disk — the pipeline streams
  through anonymous pipes inside the container, and the container root FS
  is read-only (`read_only: true` on the service), so even an inadvertent
  `>file` would fail.

If the drill fails, raise an issue against this runbook and *do not* mark
the quarter's drill as passed.

## Negative test (must fail)

Trying to `pg_restore` an encrypted dump directly must fail loudly. This
is enforced as a Go test
(`internal/backup.TestPgRestoreRejectsRawAge`) so the safety net cannot
be silently removed from the suite.

```bash
aws s3 cp "s3://$BACKUP_BUCKET/$(date -u +%F)/$NODE_ID/dump.pgc.age" /tmp/raw.age
! pg_restore --list /tmp/raw.age 2>/dev/null
# pg_restore must exit non-zero with "input file appears to be a text format
# dump. Please use psql." or "did not find magic number".
shred -u /tmp/raw.age
```

## Rotacao de chave (planned rotation)

Rotate the recipient key on a regular cadence (default: yearly) and any time
a backup host is decommissioned or compromised. Rotation does NOT re-encrypt
old dumps — they stay encrypted to the old key. Keep the old private key in
the offline secret store until the retention window for those dumps elapses.

The recommended path is a **dual-recipient transition**: ship both old and
new public keys in `infra/age-backup.pub` for one rotation cycle so either
private key decrypts new dumps. `age -R` reads every non-comment line of
the recipients file and encrypts to all of them.

> **Container-specific step ([SIN-66537](/SIN/issues/SIN-66537)).** The
> recipient is bind-mounted read-only from the host at
> `/etc/lmhost/age-backup.pub` (not baked into the signed image), so rotating
> it is a **host file swap** — edit that file, then restart the sidecar
> (`docker compose --profile backup up -d backup`) so it re-reads the mount.
> **No image rebuild or `BACKUP_IMAGE=` digest bump is required**, and the
> committed `infra/age-backup.pub` stays the placeholder (CI enforces it). See
> [ADR-0110](../adr/0110-backup-age-recipient-runtime-mount.md).

1. **On the host with sudo**, generate the new keypair:
   ```bash
   sudo BACKUP_AGE_KEY=/etc/lmhost/age-backup.key.new \
     ./scripts/generate-backup-key.sh
   ```
2. **Append** the new public key as a second line in the host recipient file
   `/etc/lmhost/age-backup.pub` — do not delete the old line yet. `age -R`
   encrypts to every non-comment line, so dumps become decryptable by either
   private key during the transition:
   ```bash
   printf '%s\n' 'age1...<new recipient>' \
     | sudo tee -a /etc/lmhost/age-backup.pub >/dev/null
   # Restart the sidecar so it re-reads the mounted recipient file:
   sudo -u crm-deploy docker compose -f /opt/crm/stg/compose.stg.yml \
     --env-file /opt/crm/stg/.env.stg --profile backup up -d backup
   ```
3. SOPS-encrypt the new private key:
   ```bash
   sudo sops --encrypt --age "$SOPS_AGE_RECIPIENT" \
        /etc/lmhost/age-backup.key.new \
        > infra/sops/age-backup.key.enc.new
   git mv infra/sops/age-backup.key.enc.new infra/sops/age-backup.key.enc
   ```
4. **Stash a second copy of the cleartext private key in the offline cofre.**
   The cofre configuration is fixed — see § Cofre offline.
5. Smoke-test by running the restore drill with **each** private key (same
   `--user 0:0` + `PG*` env-var pattern as the quarterly drill above):
   ```bash
   # old key
   sudo -u crm-deploy docker compose -f /opt/crm/stg/compose.stg.yml \
     --env-file /opt/crm/stg/.env.stg run --rm \
       --user 0:0 \
       --entrypoint /usr/local/bin/backup-restore.sh \
       -v /etc/lmhost/age-backup.key:/etc/lmhost/age-backup.key:ro \
       -e BACKUP_AGE_KEY=/etc/lmhost/age-backup.key \
       -e PGHOST=postgres -e PGPORT=5432 -e PGDATABASE=scratch \
       -e PGUSER=drill -e PGPASSWORD="$DRILL_PASSWORD" \
       backup

   # new key
   sudo -u crm-deploy docker compose -f /opt/crm/stg/compose.stg.yml \
     --env-file /opt/crm/stg/.env.stg run --rm \
       --user 0:0 \
       --entrypoint /usr/local/bin/backup-restore.sh \
       -v /etc/lmhost/age-backup.key.new:/etc/lmhost/age-backup.key:ro \
       -e BACKUP_AGE_KEY=/etc/lmhost/age-backup.key \
       -e PGHOST=postgres -e PGPORT=5432 -e PGDATABASE=scratch \
       -e PGUSER=drill -e PGPASSWORD="$DRILL_PASSWORD" \
       backup
   ```
6. **One retention window later**, when every dump in the bucket can be
   decrypted by the new key alone, swap atomically and drop the old line:
   ```bash
   sudo install -m 0440 -o root -g lmhost-backup \
        /etc/lmhost/age-backup.key.new /etc/lmhost/age-backup.key
   sudo rm /etc/lmhost/age-backup.key.new
   ```
   Edit the host recipient file `/etc/lmhost/age-backup.pub` to remove the old
   recipient line, then restart the sidecar so it re-reads the mount
   (`docker compose --profile backup up -d backup`). No image rebuild or
   `BACKUP_IMAGE=` bump is required.
7. Continue to retain the old private key in the cofre until the longest
   retention window for any dump still encrypted to it has elapsed.

If you must do a hard cutover (incident: the old key is compromised), skip
the dual-recipient phase: replace the line in the host recipient file
`/etc/lmhost/age-backup.pub`, restart the sidecar, run a backup against the
new recipient, and treat any old dumps as forensic evidence to be decrypted
only under controlled conditions.

## Cofre offline (2nd-tier secret store)

The offline copy of the private key is the single thing that turns the
catastrophic-loss scenario into a recoverable one. The configuration is
**fixed** (decision in [SIN-62261](/SIN/issues/SIN-62261)) — not a menu of
options — and is the same for every host:

- **Storage stack:** [KeePassXC](https://keepassxc.org) `.kdbx` database
  (AES-256 cipher, Argon2id KDF) **inside** an encrypted volume:
  - **LUKS** (Linux) — preferred when the custódio operates from a Linux
    host; OR
  - **VeraCrypt** — cross-platform fallback when the custódio operates
    from a non-Linux host.
- **Container:** a **dedicated USB stick** that holds the encrypted
  volume, the KeePassXC DB, and a plain-text copy of the matching public
  recipient (`age-backup.pub`) for verification — and **nothing else**.
  No other documents, no spare backups, no day-to-day files.
- **Localização física:** the USB MUST be stored **outside the custódio's
  primary residence** — family member, trusted relative, lawyer's
  office, or postal box are all acceptable. The USB MUST NOT live on the
  dev laptop. Co-locating the cofre with the dev laptop nullifies the
  defense against the joint scenario "AWS credential compromised AND
  dev laptop compromised".
- **Custódia primária:** Pericles Luz.
- **Plano B (catastrophic):** sealed envelope handed to a trusted party
  located outside the primary residence. See § Chave perdida for the
  retrieval procedure. Trusted-party identity is captured operationally
  with the placeholder `<TBD: Pericles preenche antes de Fase 4 prod cutoff>`.
- **4-eyes:** not satisfied by design — LMHost is a single-founder
  org during Fase 0–4. The formal multi-person procedure activates when
  a 2ª pessoa-chave is hired (see § Offboarding).

Record the operational state in this runbook (review during the
quarterly audit; see § Auditoria trimestral):

| Field | Value |
|-------|-------|
| Storage stack | KeePassXC `.kdbx` inside LUKS (or VeraCrypt) on dedicated USB |
| USB serial | _e.g. `Kingston DT 50 A1B2C3...`_ |
| Custódio primário | Pericles Luz |
| Localização física | `<TBD: Pericles preenche antes de Fase 4 prod cutoff>` |
| Trusted party (Plano B) | `<TBD: Pericles preenche antes de Fase 4 prod cutoff>` |
| Last verified | _YYYY-MM-DD_ (atualizado pela auditoria trimestral) |

> **Operator action — must be filled before Fase 4 prod cutoff.** If the
> table above still contains placeholders at cutoff, the encrypted-backup
> pipeline is not ready for production traffic. Block the Fase 4 sign-off.

Verification protocol (quarterly): see § Auditoria trimestral. The audit
exercises the cofre by mounting the volume, exporting the key, decrypting
a synthetic ciphertext, and re-sealing — then updates *Last verified*
above.

## Chave perdida (catastrophic loss)

The recovery path depends on which copies survive. Try the procedures in
order; only when **every** path below fails is the loss truly catastrophic.

### Plano A — primary cofre intact

The custódio primário (Pericles Luz) has the dedicated USB. Mount the
LUKS/VeraCrypt volume, open the KeePassXC `.kdbx`, export
`age-backup.key` to a temp file, and run the restore per § Restore drill.
This is the normal recovery flow and is exercised end-to-end during the
quarterly audit (see § Auditoria trimestral).

### Plano B — primary cofre destroyed, sealed envelope retrieval (B1)

The sealed envelope handed to a trusted party located outside the primary
residence is the second-tier offline copy. Trusted-party identity is
captured operationally; until the placeholder `<TBD: Pericles preenche
antes de Fase 4 prod cutoff>` in § Cofre offline is filled in, this
procedure is **non-functional** and Fase 4 prod cutoff is blocked.

Retrieval procedure (B1):

1. Pericles contacts the trusted party (identity per § Cofre offline) and
   coordinates an in-person hand-off.
2. Pericles retrieves the sealed envelope. Tamper evidence on the seal is
   inspected and recorded; a broken or modified seal escalates to a
   critical incident regardless of whether retrieval succeeds.
3. Open the envelope **in the presence of a witness** when feasible. The
   single-founder configuration may waive the witness requirement when no
   second person-key exists; record the waiver in the post-mortem.
4. Use the recorded passphrase to unlock the LUKS/VeraCrypt volume and
   the KeePassXC DB.
5. Export `age-backup.key` to a temp file (RAM-backed `tmpfs` preferred).
6. Run the restore per § Restore drill (Fase 6) against the target dump.
7. `shred -u` the temp file, prepare a fresh sealed envelope, and rotate
   to a new trusted-party hand-off slot before closing the incident.

> **4-eyes is not satisfied by design** in Fase 0–4 — LMHost is a
> single-founder org. The formal multi-person retrieval procedure
> activates when a 2ª pessoa-chave is hired; see § Offboarding for the
> activation trigger.

### Plano C — both cofre copies destroyed, key truly lost

If the recipient private key is lost AND **neither** offline copy exists
(USB destroyed AND sealed envelope unavailable):

- **Every dump encrypted to that key is unrecoverable.** Do not pretend
  otherwise.
- Open a critical incident.
- Pivot to the most recent recoverable source: streaming replica, WAL
  archive, or app-level export. Restore drills should already have
  validated those.
- Generate a new keypair (see § Rotação de chave) and start producing
  fresh encrypted backups immediately — every additional hour without an
  off-site backup compounds exposure.
- Run a full post-mortem: how did **all** copies disappear? The offline
  cofre + Plano B envelope exist specifically to make this scenario
  unreachable; understand why neither saved us.

## Auditoria trimestral

Cadence: **first Monday of January, April, July, and October**. The audit
is single-person by design (single-founder org during Fase 0–4) and
validates that the cofre configuration still matches policy AND that the
offline private key still decrypts a fresh ciphertext.

Procedure (6 steps):

1. **CTO routine** opens the quarterly audit issue with this checklist
   pre-filled and assigns it to Pericles.
2. **Pericles** mounts the LUKS/VeraCrypt volume, opens the KeePassXC
   `.kdbx`, and exports `age-backup.key` to a temp file (`tmpfs`
   preferred, e.g. `/dev/shm/age-backup.key`).
3. **Coder** generates a synthetic dump (~1 000 fake rows in a throwaway
   schema) and encrypts it with `infra/age-backup.pub`.
4. **Pericles** runs the round-trip against an ephemeral Postgres in the
   compose stack:

   ```bash
   age -d -i /dev/shm/age-backup.key < dump.pgc.age \
     | pg_restore --clean -d "$DB_EPHEMERAL"
   ```

   and confirms the row count matches the synthetic dump.
5. **Pericles** `shred -u`s the temp key file and posts evidence on the
   audit issue: SHA-256 of the encrypted dump, restored row count,
   timestamp, and any anomalies.
6. **Failure at any step ⇒ P0 incident.** The cofre and/or recipient key
   are considered at risk; rotate per § Rotação de chave and treat the
   quarter's audit as failed.

Update the *Last verified* row in § Cofre offline at the end of every
successful audit.

## Offboarding

> **Status:** activates when a 2ª pessoa-chave is hired. Until then: **N/A.**
> The cofre is currently a single-custódio configuration (Pericles Luz);
> there is no role to off-board.

When activated (i.e. once the org has at least two key custodians):

- **≤ 4h after departure:** revoke the departing custodian's access by
  rotating the KeePassXC master password AND removing their keyfile (or
  hardware token) from the `.kdbx` configuration.
- **≤ 24h after departure:** rotate `infra/age-backup.pub` per § Rotação
  de chave (full dual-recipient cycle when the retention window permits;
  hard-cutover if the departure was acrimonious or the key may be
  compromised).
- **Same PR as the rotation:** update the nominal custodian list in this
  runbook (§ Cofre offline) so the audit log reflects the new state.
- **Out-of-cadence audit ≤ 7d after departure** to confirm the new cofre
  configuration still decrypts production ciphertexts and that the
  departed custodian's copies are demonstrably destroyed (or accounted
  for as forensic evidence under controlled conditions).

## Threat model recap

| Vector | Mitigated? |
|--------|------------|
| AWS/B2 credential leak | yes — attacker downloads ciphertext only |
| Backup container compromised | partial — attacker has process access to the running sidecar, but the private age key is NOT mounted into the scheduled service. Compromise can stop new backups (denial), but cannot decrypt any past dump. Restore-drill invocations expose the key only for the duration of that one ad-hoc container. |
| Backup host compromised | partial — attacker can read `/etc/lmhost/age-backup.key` (perms `0440 root:lmhost-backup`); rotate + revoke immediately. |
| Repo credentials leaked (commit access) | yes — only ciphertext + public placeholder + SOPS-encrypted private key in git |
| SOPS recipient key compromised | partial — attacker can decrypt the SOPS file *if* they also have repo read access; rotate both |
| Tampering with stored dump | yes — age v1+ MACs the payload (HMAC-SHA-256). `pg_restore` fails if even one byte is flipped. |
| Confused deputy on the backup host | yes — the scheduled sidecar runs as nobody:nobody with cap_drop ALL and read-only root FS; the private key path is not mountable from inside the container. |
| Tampered `age` binary in the image | partial — image is cosign-signed via `build-backup-image.yml` and pinned by digest in `compose.stg.yml`; upstream Alpine `age` package is the source of trust. Bumping the Alpine pin is a PR-reviewed change. |

`age` v1.0+ is required (HMAC of the ciphertext). Earlier `age` releases
lack the MAC; both `backup.sh` and `backup-restore.sh` enforce this at
runtime via an `age --version` preflight, and `TestBackupScriptRejectsOldAge`
guards against accidental removal of that check.

### Defense-in-depth additions (recommended, not required)

- **Bucket-level SSE / Object Lock.** Client-side `age` is the actual
  confidentiality boundary, but enabling SSE-S3 (or B2 server-side
  encryption) plus an Object Lock retention policy adds one extra step
  for an attacker pivoting across compromised storage credentials. Apply
  during bucket provisioning; orthogonal to this runbook.
- **Pre-commit hooks.** `TestNoAgeSecretInGitHistory` is a passive
  scan that catches a leak after it lands in git. Pair it with
  pre-commit hooks (`gitleaks`, `trufflehog`) that block the commit
  before the leak ever reaches `git push`. The Go test is a guardrail,
  not a substitute for the pre-commit layer.
