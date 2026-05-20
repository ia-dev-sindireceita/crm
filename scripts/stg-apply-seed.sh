#!/usr/bin/env bash
# scripts/stg-apply-seed.sh — apply migrations/seed/stg.sql against the
# staging Postgres with the tenant FQDN substituted (SIN-63146).
#
# Expects to run on the VPS as a user that can `sudo -u crm-deploy docker
# compose …`. Docs: see docs/deploy/staging.md §5d.
#
# Required env:
#   STG_BASE_DOMAIN  — tenant FQDN suffix, e.g. "crm.someu.com.br".
#                      MUST NOT be empty and MUST NOT end in `.local`
#                      (that's the dev default and a hard-fail tripwire
#                      for accidentally seeding staging with dev hosts).
#
# Optional env (defaults match /opt/crm/stg/ layout):
#   STG_DIR          — staging stack root         (default: /opt/crm/stg)
#   STG_ENV_FILE     — env-file for compose       (default: $STG_DIR/.env.stg)
#   STG_COMPOSE_FILE — compose yaml               (default: $STG_DIR/compose.stg.yml)
#   STG_SEED_FILE    — seed SQL path on the VPS   (default: $STG_DIR/migrations/seed/stg.sql)
#   DEPLOY_USER      — constrained deploy account (default: crm-deploy)

set -euo pipefail

if [ -z "${STG_BASE_DOMAIN:-}" ]; then
  echo "stg-apply-seed: STG_BASE_DOMAIN is empty — refuse to seed without a target FQDN" >&2
  exit 64
fi
case "${STG_BASE_DOMAIN}" in
  *.local)
    echo "stg-apply-seed: STG_BASE_DOMAIN=${STG_BASE_DOMAIN} ends in '.local' — refuse to seed staging with the dev fixture domain" >&2
    exit 64
    ;;
esac

STG_DIR="${STG_DIR:-/opt/crm/stg}"
STG_ENV_FILE="${STG_ENV_FILE:-${STG_DIR}/.env.stg}"
STG_COMPOSE_FILE="${STG_COMPOSE_FILE:-${STG_DIR}/compose.stg.yml}"
STG_SEED_FILE="${STG_SEED_FILE:-${STG_DIR}/migrations/seed/stg.sql}"
DEPLOY_USER="${DEPLOY_USER:-crm-deploy}"

for f in "${STG_ENV_FILE}" "${STG_COMPOSE_FILE}" "${STG_SEED_FILE}"; do
  if [ ! -s "${f}" ]; then
    echo "stg-apply-seed: missing or empty ${f}" >&2
    exit 66
  fi
done

# Resolve POSTGRES_USER / POSTGRES_DB without sourcing the env file in
# our own shell — the .env.stg file is owned by crm-deploy and may not
# be world-readable.
read_env() {
  sudo grep -E "^${1}=" "${STG_ENV_FILE}" | tail -n 1 | cut -d= -f2-
}
POSTGRES_USER_VAL="$(read_env POSTGRES_USER)"
POSTGRES_DB_VAL="$(read_env POSTGRES_DB)"
if [ -z "${POSTGRES_USER_VAL}" ] || [ -z "${POSTGRES_DB_VAL}" ]; then
  echo "stg-apply-seed: POSTGRES_USER / POSTGRES_DB not set in ${STG_ENV_FILE}" >&2
  exit 65
fi

echo "stg-apply-seed: STG_BASE_DOMAIN=${STG_BASE_DOMAIN}"
echo "stg-apply-seed: seeding tenants acme.${STG_BASE_DOMAIN}, globex.${STG_BASE_DOMAIN}"

sudo -u "${DEPLOY_USER}" docker compose \
  --env-file "${STG_ENV_FILE}" \
  -f "${STG_COMPOSE_FILE}" \
  exec -T postgres \
  psql -v ON_ERROR_STOP=1 \
       -v base_domain="${STG_BASE_DOMAIN}" \
       -U "${POSTGRES_USER_VAL}" \
       -d "${POSTGRES_DB_VAL}" \
  < "${STG_SEED_FILE}"

echo "stg-apply-seed: ok"
