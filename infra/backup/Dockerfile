# Sindireceita backup sidecar — pg_dump | age | aws s3 cp pipeline.
#
# Built and pushed by .github/workflows/build-backup-image.yml on every push
# that touches `infra/backup/**` or `scripts/backup.sh` / `scripts/backup-restore.sh`.
# Consumed by `deploy/compose/compose.stg.yml` and `deploy/compose/compose.yml`
# as `ghcr.io/ia-dev-sindireceita/crm-backup@sha256:<digest>` (pinned inline
# like every other infra image — see compose.stg.yml header for the policy;
# fork namespace per SIN-63281).
#
# Stack:
#   - postgres-client    (pg_dump)
#   - age                (X25519 client-side encryption)
#   - aws-cli v2         (S3 / Backblaze B2 put + head-object verify)
#   - supercronic        (cron-in-container, signal-aware, no PID-1 reaping issues)
#
# Hardening defaults (mirror the legacy systemd unit primitives — see ADR
# 0102 § "Hardening invariant mapping"):
#   - USER 65534 (nobody)
#   - filesystem readonly enforced at compose-service level (read_only: true)
#   - tmpfs /tmp at compose-service level (PrivateTmp equivalent)
#   - cap_drop ALL at compose-service level (RestrictSUIDSGID equivalent)
#   - security_opt no-new-privileges at compose-service level
#
# The private age key is NEVER baked into the image and NEVER bind-mounted
# into the scheduled service; it is only mounted at backup-restore.sh
# invocation time, AND the invocation MUST pass `--user 0:0` so the
# UID/GID inside the container matches the host-side `0440 root:*`
# ownership of `/etc/sindireceita/age-backup.key`. See
# docs/operations/backup-restore.md § "Restore drill" and SIN-63195
# SE-review BLOCKER #2 for the rationale.

# Verify against the Alpine 3.20.3 manifest on docker hub. Image-pinning
# policy from SIN-62215 requires tag@digest for every infra image. If this
# digest does not match the upstream `alpine:3.20.3` manifest, the build
# fails fast (intended). SecurityEngineer rev: confirm the digest on PR
# review against `docker manifest inspect alpine:3.20.3`.
FROM alpine:3.20.3@sha256:beefdbd8a1da6d2915566fde36db9db0b524eb737fc57cd1367effd16dc0d06d

# Pinned package versions are NOT enforced here — Alpine's repository state
# is the gating mechanism for the underlying tools' versions, and the
# `--no-cache` flag keeps the image reproducible by-build-time. `age` v1.x
# is enforced by backup.sh's preflight check (`require_age_v1`), so an
# accidental v0.x rollback in the upstream repo is caught at script-start,
# not at image-build.
RUN apk add --no-cache \
      bash \
      ca-certificates \
      coreutils \
      postgresql16-client \
      age \
      aws-cli \
      tzdata

# supercronic — cron-in-container, signal-aware. Pinned to a release binary
# downloaded by URL and verified by SHA256. We deliberately avoid `apk add
# supercronic` because supercronic is not in the Alpine main/community repos
# (would force `--repository edge`), and we deliberately avoid building
# from source because it would balloon the image to include the Go toolchain.
#
# Verify against https://github.com/aptible/supercronic/releases/tag/v0.2.34
# (linux-amd64). If the SHA does not match, the build fails at the sha256sum
# step. SecurityEngineer rev: confirm the SHA on PR review against the
# release page checksum file.
ENV SUPERCRONIC_URL="https://github.com/aptible/supercronic/releases/download/v0.2.34/supercronic-linux-amd64" \
    SUPERCRONIC_SHA256="a51b340a83c5bd035742f0d7191555f9663876405e494dbf824537d64f3e39c6" \
    SUPERCRONIC_BIN="/usr/local/bin/supercronic"

# wget is in busybox so we do not need to apk add it. The sha256sum check
# fails the build if the downloaded blob does not match.
RUN wget -q -O "${SUPERCRONIC_BIN}" "${SUPERCRONIC_URL}" \
 && echo "${SUPERCRONIC_SHA256}  ${SUPERCRONIC_BIN}" | sha256sum -c - \
 && chmod 0755 "${SUPERCRONIC_BIN}"

# Copy the pipeline into a read-only location. The compose service mounts
# the source tree read-only on dev (`./scripts:/usr/local/sindireceita/scripts:ro`),
# but the production image ships its own embedded copy so the sidecar can
# run with `read_only: true` and zero host filesystem mounts (except tmpfs
# /tmp and the env file).
COPY scripts/backup.sh          /usr/local/bin/backup.sh
COPY scripts/backup-restore.sh  /usr/local/bin/backup-restore.sh
COPY infra/age-backup.pub       /opt/sindireceita/infra/age-backup.pub
COPY infra/backup/crontab       /etc/sindireceita/crontab

# Ownership: nobody:nobody (65534:65534). All paths are owned by root post-COPY
# by default; chown to nobody so the unprivileged USER below can stat them.
# The scripts MUST stay 0555 (no group/other write); the crontab is 0444.
# Note: the scheduled cron path runs as nobody (USER below). The restore
# path is invoked separately with `docker compose run --rm --user 0:0` so
# the bind-mounted age private key (host-side `0440 root:*`) is readable.
# The 0555 perms ensure either UID can `exec` the scripts.
RUN chown -R 65534:65534 /opt/sindireceita \
 && chmod 0555 /usr/local/bin/backup.sh /usr/local/bin/backup-restore.sh \
 && chmod 0444 /etc/sindireceita/crontab

# /var/lib/sindireceita is where backup.sh writes backup-last-success.json.
# Compose mounts a named volume here so the per-host state survives image
# upgrades. Inside the image we ship the directory empty + writable by the
# nobody user so first-boot does not need root.
RUN mkdir -p /var/lib/sindireceita \
 && chown -R 65534:65534 /var/lib/sindireceita

USER 65534:65534
WORKDIR /opt/sindireceita

# Default command is supercronic against the embedded crontab. The crontab
# fires `/usr/local/bin/backup.sh` at the wall-clock schedule. For a manual
# one-shot backup the operator runs `docker compose run --rm backup
# /usr/local/bin/backup.sh`. For the encrypted-restore pipeline the
# operator runs `docker compose run --rm --user 0:0 -v
# /etc/sindireceita/age-backup.key:/etc/sindireceita/age-backup.key:ro
# backup /usr/local/bin/backup-restore.sh` (the explicit `--user 0:0` is
# required so the bind-mounted age key — host perms 0440 root:* — is
# readable inside the container).
ENTRYPOINT ["/usr/local/bin/supercronic"]
CMD ["/etc/sindireceita/crontab"]
