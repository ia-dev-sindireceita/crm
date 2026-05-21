#!/usr/bin/env bash
# Restore drill for the encrypted Sindireceita backups.
#
# Pipeline: aws s3 cp | age -d -i KEY | pg_restore.
# Run by Fase 6 ([SIN-62199]) to prove the encrypt -> upload -> download ->
# decrypt -> restore loop works end-to-end. Also used as part of incident
# response when the production DB needs to be rehydrated from a known dump.
#
# Required env:
#   RESTORE_URL     libpq URL of the *ephemeral* restore target.
#   BACKUP_BUCKET   S3 bucket name (no scheme).
# Optional env:
#   RESTORE_DATE    YYYY-MM-DD (default: today UTC). Selects which snapshot.
#   RESTORE_OBJECT  fully-qualified object key under BACKUP_BUCKET; overrides
#                   RESTORE_DATE/BACKUP_PREFIX based path.
#   BACKUP_PREFIX   object key prefix used by backup.sh.
#   BACKUP_AGE_KEY  age private-key file (default:
#                   /etc/sindireceita/age-backup.key).
#   AWS_ENDPOINT_URL  custom S3 endpoint (e.g. Backblaze B2).
#   RESTORE_VERIFY_SQL  optional smoke-test query; expected to print a single
#                       integer >= RESTORE_VERIFY_MIN.
#   RESTORE_VERIFY_MIN  minimum integer the smoke query must return (default 1).
#
# Logs go to stdout/stderr; do not echo dump bytes or RESTORE_URL.
# SIN-62250 — restore drill required for Fase 6 sign-off.
set -Eeuo pipefail
shopt -s inherit_errexit

log() { printf '[restore-drill.sh] %s %s\n' "$(date -u +%FT%TZ)" "$*" >&2; }
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

: "${RESTORE_URL:?RESTORE_URL must be set (point at an EPHEMERAL DB, not prod)}"
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

log "starting restore drill <- $source_url"

# aws s3 cp - | age -d -i KEY | pg_restore. pipefail surfaces a failure at any
# stage. pg_restore --clean drops then recreates objects so the drill is
# idempotent against an already-populated ephemeral DB.
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
      --exit-on-error \
      --dbname="$RESTORE_URL"

if [[ -n "${RESTORE_VERIFY_SQL:-}" ]]; then
  min=${RESTORE_VERIFY_MIN:-1}
  log "running verify query"
  rows=$(psql --quiet --tuples-only --no-align --dbname="$RESTORE_URL" \
              --command "$RESTORE_VERIFY_SQL" | tr -d '[:space:]')
  [[ "$rows" =~ ^[0-9]+$ ]] || fail "verify query did not return an integer: $rows"
  (( rows >= min )) || fail "verify rows=$rows below minimum=$min"
  log "verify ok (rows=$rows >= $min)"
fi

log "restore drill complete <- $source_url"
