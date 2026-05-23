#!/usr/bin/env bash
# stg-deploy.sh — VPS-side deploy entrypoint for the staging CD pipeline.
# Installed at /opt/crm/stg/bin/deploy.sh per docs/deploy/staging.md.
#
# The CD SSH key in /home/<deploy-user>/.ssh/authorized_keys MUST be locked
# down with command="/opt/crm/stg/bin/deploy.sh",no-pty,no-agent-forwarding,
# no-port-forwarding,no-X11-forwarding so that this script is the ONLY thing
# the GitHub runner can invoke on the host. The remote command from the
# runner — "deploy ghcr.io/.../crm@sha256:..." — arrives via $SSH_ORIGINAL_COMMAND.
#
# Contract:
#   - Verbs:
#       deploy     <image-ref>  — update .env.stg APP_IMAGE, compose pull/up, prune.
#       migrate-up <image-ref>  — extract /migrations from <image-ref> via
#                                 docker cp and apply them against the staging
#                                 postgres using a one-shot migrate/migrate
#                                 sidecar pinned by digest. Idempotent
#                                 (golang-migrate skips already-applied
#                                 versions). Added in SIN-63332 to close the
#                                 F10 deploy-procedure gap surfaced by
#                                 PR #104.
#       preflight              — no-op health probe: asserts `cosign` is on
#                                 PATH and exits 0. Added in SIN-63350 so the
#                                 CD pipeline can detect a missing cosign
#                                 binary on the VPS BEFORE invoking deploy
#                                 (which would otherwise fail mid-step with
#                                 `stg-deploy: cosign not found in PATH`
#                                 exit 67). No image-ref required, no
#                                 side-effects.
#   - Argument: APP_IMAGE reference, MUST match ghcr.io/pericles-luz/crm@sha256:[0-9a-f]{64}
#                                    (only required for deploy/migrate-up).
#   - Effect (deploy):     updates /opt/crm/stg/.env.stg, runs `compose pull && up -d`,
#                          then prunes dangling images. Previous APP_IMAGE is recorded
#                          in /opt/crm/stg/.last-image so manual rollback can read it.
#   - Effect (migrate-up): runs `migrate -path /migrations -database ... up` against
#                          crm-stg-postgres-1 inside the compose project network.
#                          The migrations bytes come from the just-deployed image at
#                          /migrations (see Dockerfile crm-server stage).
#   - Effect (preflight):  reports cosign presence on PATH (exit 0) or missing
#                          (exit 67). Read-only, never touches compose/env/DB.
#   - On any error: exits non-zero (CD job goes red, NO automatic rollback).

set -euo pipefail

readonly STG_DIR="/opt/crm/stg"
readonly ENV_FILE="${STG_DIR}/.env.stg"
readonly COMPOSE_FILE="${STG_DIR}/compose.stg.yml"
readonly LAST_IMAGE_FILE="${STG_DIR}/.last-image"
readonly EXPECTED_REPO="ghcr.io/pericles-luz/crm"
readonly DIGEST_RE="^${EXPECTED_REPO}@sha256:[0-9a-f]{64}$"

# SIN-63332 — pin the migrate/migrate image by manifest-list digest so a
# Docker Hub-side tag swap cannot replace the binary that runs against
# crm-stg-postgres-1. v4.17.1 matches the local Makefile (`MIGRATE_IMAGE`)
# so devs and the staging pipeline use the same golang-migrate. Bump the
# tag + digest together; resolve with:
#   docker buildx imagetools inspect migrate/migrate:vX.Y.Z \
#     --format '{{ .Manifest.Digest }}'
# Override only via `${MIGRATE_IMAGE_REF}=` (break-glass / test harness).
: "${MIGRATE_IMAGE_REF:=migrate/migrate:v4.17.1@sha256:de154de4b7f9d0d751aacb1ec6023f5fc96f874eb88b65d050f761a33376aa4b}"

# ADR 0084 / SIN-62247 — cosign keyless verify gate.
# The image MUST carry a Sigstore signature minted by our GitHub workflow
# (identity binding) before docker compose pull is allowed to fetch it.
# Override only via `${COSIGN}=` (test harness) or by editing this script
# (PR-reviewed break-glass). There is no --skip-verify flag.
#
# Identity regex is pinned to the `crm` repository, not to the entire
# `pericles-luz/*` namespace, so a compromise of any other repo under the
# same owner cannot mint a signature that satisfies this gate. The literal
# dots in `github.com` are escaped to remove the parser-differential class
# where `.` (any char) would match `githubXcom`. Org migration to
# `Sindireceita` is tracked in SIN-62322.
: "${COSIGN:=cosign}"
: "${COSIGN_IDENTITY_REGEXP:=^https://github\.com/pericles-luz/crm/}"
: "${COSIGN_OIDC_ISSUER:=https://token.actions.githubusercontent.com}"

# Parse the original SSH command if invoked via authorized_keys command="..."
# constraint, otherwise accept positional arg (manual run).
if [[ -n "${SSH_ORIGINAL_COMMAND:-}" ]]; then
  # shellcheck disable=SC2206  # intentional word-split of remote command
  argv=( ${SSH_ORIGINAL_COMMAND} )
else
  argv=( "$@" )
fi

# SIN-63350 — preflight verb dispatches BEFORE the {deploy|migrate-up}
# image-ref argv validation because it deliberately takes no image-ref.
# The cd-stg workflow SSHes `preflight` ahead of `deploy` so we fail red
# with an actionable remediation when cosign is missing on the VPS PATH
# (typical: cosign at /usr/local/bin/cosign but the non-interactive SSH
# command="…" PATH excludes it). Read-only, never touches compose/env/DB.
if [[ "${#argv[@]}" -eq 1 && "${argv[0]}" == "preflight" ]]; then
  if ! command -v "${COSIGN}" >/dev/null 2>&1; then
    echo "stg-deploy: preflight FAILED — ${COSIGN} not found in PATH (see docs/deploy/staging.md §VPS-bootstrap)" >&2
    exit 67
  fi
  echo "stg-deploy: preflight OK — cosign present"
  exit 0
fi

if [[ "${#argv[@]}" -ne 2 ]] || \
   [[ "${argv[0]}" != "deploy" && "${argv[0]}" != "migrate-up" ]]; then
  echo "stg-deploy: usage: {deploy|migrate-up} <image-ref> | preflight" >&2
  exit 64
fi

readonly VERB="${argv[0]}"
readonly NEW_IMAGE="${argv[1]}"

if [[ ! "${NEW_IMAGE}" =~ ${DIGEST_RE} ]]; then
  echo "stg-deploy: refusing image ref ${NEW_IMAGE}" >&2
  echo "stg-deploy: must match ${EXPECTED_REPO}@sha256:<64 hex>" >&2
  exit 65
fi

# ADR 0084 §2 — verify the cosign signature BEFORE compose pull. A missing
# or invalid signature aborts the deploy; compose is never invoked. This is
# what catches a registry-side image swap between push and pull.
# The same gate fronts `migrate-up` so a tampered image cannot smuggle
# malicious SQL into the staging DB via /migrations.
if ! command -v "${COSIGN}" >/dev/null 2>&1; then
  echo "stg-deploy: ${COSIGN} not found in PATH — install cosign >= v2.4 (see docs/deploy/staging.md)" >&2
  exit 67
fi
echo "stg-deploy: cosign verify ${NEW_IMAGE}"
if ! "${COSIGN}" verify \
    --certificate-identity-regexp "${COSIGN_IDENTITY_REGEXP}" \
    --certificate-oidc-issuer "${COSIGN_OIDC_ISSUER}" \
    "${NEW_IMAGE}" >/dev/null; then
  echo "stg-deploy: cosign verify FAILED for ${NEW_IMAGE} — refusing to ${VERB}" >&2
  exit 68
fi
echo "stg-deploy: signature OK"

if [[ ! -f "${ENV_FILE}" ]]; then
  echo "stg-deploy: ${ENV_FILE} missing — run provisioning checklist first" >&2
  exit 66
fi

if [[ ! -f "${COMPOSE_FILE}" ]]; then
  echo "stg-deploy: ${COMPOSE_FILE} missing — run provisioning checklist first" >&2
  exit 66
fi

# Reusable compose args — both verbs need to read .env.stg through compose.
readonly COMPOSE_ARGS=( --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" )

# SIN-63332 — migrate-up branches off here. Everything below the `if`
# is deploy-only; the migrate-up verb runs its own short pipeline and
# exits before touching APP_IMAGE / compose pull.
if [[ "${VERB}" == "migrate-up" ]]; then
  # Resolve compose project network so the migrate sidecar can reach the
  # in-stack `postgres` hostname. Same resolution the staging.md runbook
  # uses for the manual procedure (§5c) — empty postgres_cid means the
  # staging stack is not running, which we surface with a clear exit.
  postgres_cid="$(docker compose "${COMPOSE_ARGS[@]}" ps -q postgres)"
  if [[ -z "${postgres_cid}" ]]; then
    echo "stg-deploy: postgres container not running — cannot migrate-up" >&2
    exit 71
  fi
  network="$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}' "${postgres_cid}" | head -n1)"
  if [[ -z "${network}" ]]; then
    echo "stg-deploy: could not resolve compose network for postgres" >&2
    exit 71
  fi

  # Read DSN parts from .env.stg without sourcing the file (keeps the rest
  # of the env, in particular APP_IMAGE / MINIO_*, out of this shell).
  read_env() { grep -E "^${1}=" "${ENV_FILE}" | tail -n1 | cut -d= -f2-; }
  pg_user="$(read_env POSTGRES_USER)"
  pg_pass="$(read_env POSTGRES_PASSWORD)"
  pg_db="$(read_env POSTGRES_DB)"
  if [[ -z "${pg_user}" || -z "${pg_pass}" || -z "${pg_db}" ]]; then
    echo "stg-deploy: ${ENV_FILE} missing POSTGRES_USER/POSTGRES_PASSWORD/POSTGRES_DB" >&2
    exit 72
  fi

  # Extract /migrations from the just-deployed image into a tmpdir and
  # mount it read-only into the migrate sidecar. The image was already
  # cosign-verified above, so the bytes we are about to feed migrate are
  # the same bytes the runtime serves. Using `docker cp` (no pull) is
  # safe because compose pull during `deploy` already cached the layers.
  workdir="$(mktemp -d /tmp/stg-migrations.XXXXXX)"
  # shellcheck disable=SC2064  # we want $workdir captured at trap-install time
  trap "rm -rf '${workdir}'" EXIT
  carrier="$(docker create "${NEW_IMAGE}")"
  trap "rm -rf '${workdir}'; docker rm -f '${carrier}' >/dev/null 2>&1 || true" EXIT
  docker cp "${carrier}:/migrations" "${workdir}/migrations"
  docker rm -f "${carrier}" >/dev/null
  trap "rm -rf '${workdir}'" EXIT
  if [[ ! -d "${workdir}/migrations" ]] || [[ -z "$(ls -A "${workdir}/migrations" 2>/dev/null)" ]]; then
    echo "stg-deploy: /migrations missing or empty inside ${NEW_IMAGE}" >&2
    exit 73
  fi

  echo "stg-deploy: migrate-up ${NEW_IMAGE} (image=${MIGRATE_IMAGE_REF}, db=${pg_db})"
  # `migrate ... up` is idempotent: already-applied versions are skipped.
  # We DO NOT print the DSN because $pg_pass would land in the log.
  # The migrate binary itself never echoes the DSN on success.
  docker run --rm \
    --network "${network}" \
    -v "${workdir}/migrations:/migrations:ro" \
    "${MIGRATE_IMAGE_REF}" \
    -path /migrations \
    -database "postgres://${pg_user}:${pg_pass}@postgres:5432/${pg_db}?sslmode=disable" \
    up
  echo "stg-deploy: migrate-up OK"
  exit 0
fi

# Capture the previous APP_IMAGE so a human can roll back without grepping.
prev=""
if grep -q '^APP_IMAGE=' "${ENV_FILE}" 2>/dev/null; then
  prev="$(grep '^APP_IMAGE=' "${ENV_FILE}" | tail -n1 | cut -d= -f2-)"
fi
if [[ -n "${prev}" ]]; then
  printf '%s\n' "${prev}" > "${LAST_IMAGE_FILE}"
fi

# Atomically rewrite APP_IMAGE in the env file. Sed with a tmp file keeps the
# rest of the env (POSTGRES_*, MINIO_*, HSTS_MAX_AGE, …) untouched.
tmp="$(mktemp "${ENV_FILE}.XXXXXX")"
trap 'rm -f "${tmp}"' EXIT
if grep -q '^APP_IMAGE=' "${ENV_FILE}"; then
  sed "s|^APP_IMAGE=.*|APP_IMAGE=${NEW_IMAGE}|" "${ENV_FILE}" > "${tmp}"
else
  cp "${ENV_FILE}" "${tmp}"
  printf '\nAPP_IMAGE=%s\n' "${NEW_IMAGE}" >> "${tmp}"
fi
chmod --reference="${ENV_FILE}" "${tmp}"
mv "${tmp}"  "${ENV_FILE}"
trap - EXIT

cd "${STG_DIR}"

# COMPOSE_ARGS was declared near the verb dispatch. The same env-file +
# compose-file pair is required here because compose's variable
# interpolation only auto-loads a file literally named `.env`, while ours
# is `.env.stg`. Without --env-file, the `${MINIO_ROOT_USER:?…}` /
# `${POSTGRES_PASSWORD:?…}` placeholders in compose.stg.yml fail with
# "required variable … is missing a value", even though the app service's
# env_file: directive picks the same file up at runtime — env_file feeds
# containers, --env-file feeds compose itself.
docker compose "${COMPOSE_ARGS[@]}" pull
docker compose "${COMPOSE_ARGS[@]}" up -d --remove-orphans
docker system prune -af --volumes=false

echo "stg-deploy: ${NEW_IMAGE} live (previous: ${prev:-<none>})"
