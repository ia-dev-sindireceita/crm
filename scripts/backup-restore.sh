#!/usr/bin/env bash
# Encrypted-backup restore — the inner pipeline.
#
# Pipeline: aws s3 cp | age -d -i KEY | pg_restore.
# Run by Fase 6 ([SIN-62199]) to prove the encrypt -> upload -> download ->
# decrypt -> restore loop works end-to-end. Also used as part of incident
# response when the production DB needs to be rehydrated from a known dump.
#
# NOTE on naming: this script is the inner decrypt-and-pg_restore step.
# The outer quarterly *drill orchestrator* lives at scripts/restore-drill.sh
# (introduced in SIN-63187 / PR #226) and handles the surrounding plumbing
# (provision drill stack, validate /health, write the dated drill report).
# The two scripts compose — the orchestrator can call this script's pipeline
# for the real (non-synthetic) decryption path.
#
# Required env:
#   BACKUP_BUCKET     S3 bucket name (no scheme).
# Optional env — restore target:
#   PGHOST, PGPORT, PGDATABASE, PGUSER, PGPASSWORD
#                     Standard libpq env vars (preferred). pg_restore reads
#                     them automatically and they are NEVER visible in
#                     `ps aux` (unlike `--dbname=postgres://user:pw@host`
#                     which leaks the password on the cmdline).
#                     SecurityEngineer 2026-05-21 MEDIUM #1 (SIN-63195).
#   RESTORE_URL       libpq URL pointing at the *ephemeral* restore target.
#                     Accepted for backward compatibility with the legacy
#                     pre-fork-reset script; if set, the PG* env vars above
#                     are derived from it. Prefer the PG* split for new
#                     callers — argv leak via `pg_restore --dbname=$URL` is
#                     a real `ps aux` channel even with a non-root user.
#   RESTORE_DATE      YYYY-MM-DD (default: today UTC). Selects which snapshot.
#   RESTORE_OBJECT    fully-qualified object key under BACKUP_BUCKET; overrides
#                     RESTORE_DATE/BACKUP_PREFIX based path.
#   BACKUP_PREFIX     object key prefix used by backup.sh.
#   BACKUP_AGE_KEY    age private-key file (default:
#                     /etc/sindireceita/age-backup.key).
#   AWS_ENDPOINT_URL  custom S3 endpoint (e.g. Backblaze B2).
#   RESTORE_VERIFY_SQL  optional smoke-test query; expected to print a single
#                       integer >= RESTORE_VERIFY_MIN.
#   RESTORE_VERIFY_MIN  minimum integer the smoke query must return (default 1).
#
# Logs go to stdout/stderr; do not echo dump bytes, RESTORE_URL, or any PG*
# password.
# SIN-62250 — restore drill required for Fase 6 sign-off.
# SIN-63195 — argv-leak hardening + container invocation guidance.
set -Eeuo pipefail
shopt -s inherit_errexit

log() { printf '[backup-restore.sh] %s %s\n' "$(date -u +%FT%TZ)" "$*" >&2; }
fail() { log "ERROR: $*"; exit 1; }

# require_age_v1 aborts unless `age --version` reports a major version >= 1.
# Mirror of the guard in backup.sh. v0.x age binaries lack the HMAC, so a
# tampered ciphertext could decrypt to garbage and pg_restore would then
# choke on bytes that look corrupt rather than ciphertext that's wrong.
require_age_v1() {
  local raw major
  raw=$(age --version 2>/dev/null | head -1)
  raw=${raw#v}
  major=${raw%%.*}
  case "$major" in
    ''|*[!0-9]*) fail "could not parse 'age --version' output: ${raw:-<empty>}" ;;
  esac
  if (( major < 1 )); then
    fail "age >= 1.0 required (got: $raw); v0.x lacks HMAC tamper protection"
  fi
}

trap 'fail "command failed at line $LINENO"' ERR

# Parse RESTORE_URL into PG* env vars if it is set and the explicit PG* are
# not. Single-source the libpq target so pg_restore + psql below read
# everything from env (NEVER from argv). The split is done in pure bash so
# the URL never leaves this script's process — no curl, no python, no jq.
#
# Accepted shape (libpq URI):
#   postgres://user:password@host:port/database?param=value
# We tolerate the alternative scheme `postgresql://`. Path/query parsing
# follows RFC 3986 conventions enough for libpq — we do not URL-decode the
# password since libpq itself accepts the percent-encoded form via env.
parse_restore_url() {
  local url=$1
  [[ "$url" =~ ^postgres(ql)?://(([^:@/]*)(:([^@/]*))?@)?([^:/?]+)(:([0-9]+))?(/([^?]*))?(\?(.*))?$ ]] \
    || fail "RESTORE_URL is not a libpq URI: $url"
  : "${PGUSER:=${BASH_REMATCH[3]}}"
  : "${PGPASSWORD:=${BASH_REMATCH[5]}}"
  : "${PGHOST:=${BASH_REMATCH[6]}}"
  : "${PGPORT:=${BASH_REMATCH[8]}}"
  : "${PGDATABASE:=${BASH_REMATCH[10]}}"
  # We intentionally drop the ?query (sslmode=…). Operators that need
  # non-default sslmode export PGSSLMODE explicitly — that is the only
  # libpq channel that survives env-only invocation cleanly. The legacy
  # URL form's `?sslmode=require` was already implicit at the libpq layer
  # via PGSSLMODE / pg_service.conf.
  export PGUSER PGPASSWORD PGHOST PGPORT PGDATABASE
}

# At least one of RESTORE_URL or PGHOST+PGDATABASE must be set so we know
# where to restore.
if [[ -n "${RESTORE_URL:-}" ]]; then
  parse_restore_url "$RESTORE_URL"
  unset RESTORE_URL  # ensure the URL form does not leak into any subprocess argv
fi
: "${PGHOST:?PGHOST or RESTORE_URL must be set — point at an EPHEMERAL DB, not prod}"
: "${PGDATABASE:?PGDATABASE or RESTORE_URL must be set}"
: "${BACKUP_BUCKET:?BACKUP_BUCKET must be set}"

require_age_v1

key_file=${BACKUP_AGE_KEY:-/etc/sindireceita/age-backup.key}
[[ -r "$key_file" ]] || fail "age key file not readable: $key_file"

if [[ -n "${RESTORE_OBJECT:-}" ]]; then
  object="$RESTORE_OBJECT"
else
  prefix=${BACKUP_PREFIX:-}
  date_dir=${RESTORE_DATE:-$(date -u +%F)}
  # Default to the same per-host layout backup.sh writes. Operators
  # restoring on a different node should set BACKUP_NODE_ID (or pin
  # RESTORE_OBJECT) to the originating node id.
  node_id=${BACKUP_NODE_ID:-$(hostname -s)}
  [[ -n "$node_id" ]] || fail "could not resolve node id (hostname -s empty); set BACKUP_NODE_ID or RESTORE_OBJECT explicitly"
  object="${prefix:+$prefix/}$date_dir/$node_id/dump.pgc.age"
fi
source_url="s3://${BACKUP_BUCKET}/${object}"

aws_extra=()
if [[ -n "${AWS_ENDPOINT_URL:-}" ]]; then
  aws_extra+=(--endpoint-url "$AWS_ENDPOINT_URL")
fi

log "starting restore <- $source_url"

# aws s3 cp - | age -d -i KEY | pg_restore. pipefail surfaces a failure at any
# stage. pg_restore --clean drops then recreates objects so the operation is
# idempotent against an already-populated ephemeral DB.
#
# pg_restore reads PGHOST/PGPORT/PGDATABASE/PGUSER/PGPASSWORD from env — we
# explicitly do NOT pass --dbname=$URL because that would put the password
# on argv where `ps aux` can read it. SIN-63195 MEDIUM #1.
aws s3 cp \
    "${aws_extra[@]}" \
    --no-progress \
    "$source_url" \
    - \
  | age -d -i "$key_file" \
  | pg_restore \
      --clean \
      --if-exists \
      --no-owner \
      --no-privileges \
      --exit-on-error

if [[ -n "${RESTORE_VERIFY_SQL:-}" ]]; then
  min=${RESTORE_VERIFY_MIN:-1}
  log "running verify query"
  # Same env-only invocation as pg_restore: psql reads PG* from env, never
  # the URL on argv.
  rows=$(psql --quiet --tuples-only --no-align \
              --command "$RESTORE_VERIFY_SQL" | tr -d '[:space:]')
  [[ "$rows" =~ ^[0-9]+$ ]] || fail "verify query did not return an integer: $rows"
  (( rows >= min )) || fail "verify rows=$rows below minimum=$min"
  log "verify ok (rows=$rows >= $min)"
fi

log "restore complete <- $source_url"
