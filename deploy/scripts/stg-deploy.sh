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
#   - Argument: APP_IMAGE reference, MUST match ghcr.io/ia-dev-sindireceita/crm@sha256:[0-9a-f]{64}
#   - Effect:   updates /opt/crm/stg/.env.stg, runs `compose pull && up -d`,
#               then prunes dangling images. Previous APP_IMAGE is recorded
#               in /opt/crm/stg/.last-image so manual rollback can read it.
#   - On any error: exits non-zero (CD job goes red, NO automatic rollback).
#
# SIN-63281 (2026-05-22): EXPECTED_REPO and the cosign identity regex
# moved from `pericles-luz/crm` to `ia-dev-sindireceita/crm` together. The
# fork's GITHUB_TOKEN can only write packages under its own owner, so
# cd-stg.yml pushes the image under the fork namespace, the cosign
# signature identifies the fork's workflow, and this verify gate must
# accept that binding. After bumping this file, re-`scp` it to
# /opt/crm/stg/bin/deploy.sh on the VPS per docs/deploy/staging.md §4.
# Org migration of the upstream to `Sindireceita` is tracked in SIN-62322.

set -euo pipefail

readonly STG_DIR="/opt/crm/stg"
readonly ENV_FILE="${STG_DIR}/.env.stg"
readonly COMPOSE_FILE="${STG_DIR}/compose.stg.yml"
readonly LAST_IMAGE_FILE="${STG_DIR}/.last-image"
readonly EXPECTED_REPO="ghcr.io/ia-dev-sindireceita/crm"
readonly DIGEST_RE="^${EXPECTED_REPO}@sha256:[0-9a-f]{64}$"

# ADR 0084 / SIN-62247 — cosign keyless verify gate.
# The image MUST carry a Sigstore signature minted by our GitHub workflow
# (identity binding) before docker compose pull is allowed to fetch it.
# Override only via `${COSIGN}=` (test harness) or by editing this script
# (PR-reviewed break-glass). There is no --skip-verify flag.
#
# Identity regex is pinned to the `crm` repository under the fork owner
# (ia-dev-sindireceita), not to the entire owner namespace, so a compromise
# of any other repo under the same owner cannot mint a signature that
# satisfies this gate. The literal dots in `github.com` are escaped to
# remove the parser-differential class where `.` (any char) would match
# `githubXcom`. Org migration of the upstream to `Sindireceita` is
# tracked in SIN-62322.
: "${COSIGN:=cosign}"
: "${COSIGN_IDENTITY_REGEXP:=^https://github\.com/ia-dev-sindireceita/crm/}"
: "${COSIGN_OIDC_ISSUER:=https://token.actions.githubusercontent.com}"

# Parse the original SSH command if invoked via authorized_keys command="..."
# constraint, otherwise accept positional arg (manual run).
if [[ -n "${SSH_ORIGINAL_COMMAND:-}" ]]; then
  # shellcheck disable=SC2206  # intentional word-split of remote command
  argv=( ${SSH_ORIGINAL_COMMAND} )
else
  argv=( "$@" )
fi

if [[ "${#argv[@]}" -ne 2 || "${argv[0]}" != "deploy" ]]; then
  echo "stg-deploy: usage: deploy <image-ref>" >&2
  exit 64
fi

readonly NEW_IMAGE="${argv[1]}"

if [[ ! "${NEW_IMAGE}" =~ ${DIGEST_RE} ]]; then
  echo "stg-deploy: refusing image ref ${NEW_IMAGE}" >&2
  echo "stg-deploy: must match ${EXPECTED_REPO}@sha256:<64 hex>" >&2
  exit 65
fi

# ADR 0084 §2 — verify the cosign signature BEFORE compose pull. A missing
# or invalid signature aborts the deploy; compose is never invoked. This is
# what catches a registry-side image swap between push and pull.
if ! command -v "${COSIGN}" >/dev/null 2>&1; then
  echo "stg-deploy: ${COSIGN} not found in PATH — install cosign >= v2.4 (see docs/deploy/staging.md)" >&2
  exit 67
fi
echo "stg-deploy: cosign verify ${NEW_IMAGE}"
if ! "${COSIGN}" verify \
    --certificate-identity-regexp "${COSIGN_IDENTITY_REGEXP}" \
    --certificate-oidc-issuer "${COSIGN_OIDC_ISSUER}" \
    "${NEW_IMAGE}" >/dev/null; then
  echo "stg-deploy: cosign verify FAILED for ${NEW_IMAGE} — refusing to deploy" >&2
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

# --env-file is required: compose's variable interpolation only auto-loads a
# file literally named .env, but ours is .env.stg. Without --env-file, the
# `${MINIO_ROOT_USER:?…}` / `${POSTGRES_PASSWORD:?…}` placeholders in
# compose.stg.yml fail with "required variable … is missing a value", even
# though the app service's env_file: directive picks the same file up at
# runtime — env_file feeds containers, --env-file feeds compose itself.
readonly COMPOSE_ARGS=( --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" )
docker compose "${COMPOSE_ARGS[@]}" pull
docker compose "${COMPOSE_ARGS[@]}" up -d --remove-orphans
docker system prune -af --volumes=false

echo "stg-deploy: ${NEW_IMAGE} live (previous: ${prev:-<none>})"
