#!/usr/bin/env bash
# Encrypted Postgres backup for Sindireceita.
#
# Pipeline: pg_dump (custom format) | age (X25519 recipient) | aws s3 cp.
# The dump is encrypted client-side BEFORE it ever touches the bucket so a
# leaked AWS/B2 credential cannot decrypt the dumps.
#
# Required env:
#   DATABASE_URL    libpq URL of the source DB.
#   BACKUP_BUCKET   S3 bucket name (no scheme, no trailing slash).
# Optional env:
#   BACKUP_AGE_RECIPIENTS  recipients file (default: infra/age-backup.pub).
#   BACKUP_PREFIX          object key prefix (default: empty -> dated dir).
#   AWS_ENDPOINT_URL       custom S3 endpoint (e.g. Backblaze B2).
#
# Logs go to stdout/stderr; systemd captures them in journalctl. Do not echo
# secrets, dump bytes, or DATABASE_URL — the journal is on the same host that
# can already read the DB, but assume someone forwards journalctl to a less
# trusted SIEM.
#
# SIN-62250.
set -Eeuo pipefail
shopt -s inherit_errexit

log() { printf '[backup.sh] %s %s\n' "$(date -u +%FT%TZ)" "$*" >&2; }
fail() { log "ERROR: $*"; exit 1; }

trap 'fail "command failed at line $LINENO"' ERR

# require_age_v1 aborts unless `age --version` reports a major version >= 1.
# Earlier age releases lack the HMAC over the ciphertext, so tampering would
# go undetected. The Go test suite proves the library has the MAC; this
# preflight ensures the *system binary* used in the pipeline does too.
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

: "${DATABASE_URL:?DATABASE_URL must be set}"
: "${BACKUP_BUCKET:?BACKUP_BUCKET must be set}"

require_age_v1

# Resolve the recipients file relative to the script unless the caller pinned
# an absolute path via BACKUP_AGE_RECIPIENTS. age -R skips '#' comments, so we
# can keep the rotation/runbook header inside infra/age-backup.pub.
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null && pwd)
repo_root=$(cd -- "$script_dir/.." >/dev/null && pwd)
recipients=${BACKUP_AGE_RECIPIENTS:-"$repo_root/infra/age-backup.pub"}
[[ -r "$recipients" ]] || fail "recipients file not readable: $recipients"

prefix=${BACKUP_PREFIX:-}
date_dir=$(date -u +%F)
# Per-host segregation in the object key avoids two backup hosts overwriting
# each other if the fleet ever grows beyond one node. BACKUP_NODE_ID lets the
# operator pin a stable identifier (e.g. an inventory tag) so a hostname
# rename does not orphan history.
node_id=${BACKUP_NODE_ID:-$(hostname -s)}
[[ -n "$node_id" ]] || fail "could not resolve node id (hostname -s empty); set BACKUP_NODE_ID explicitly"
object="${prefix:+$prefix/}$date_dir/$node_id/dump.pgc.age"
target="s3://${BACKUP_BUCKET}/${object}"

aws_extra=()
if [[ -n "${AWS_ENDPOINT_URL:-}" ]]; then
  aws_extra+=(--endpoint-url "$AWS_ENDPOINT_URL")
fi

log "starting backup -> $target (recipients=$recipients)"

# pg_dump | age -R | aws s3 cp - ...
# pipefail surfaces any non-zero stage. --no-progress avoids carriage-return
# noise in journalctl. age streams stdin->stdout, so the dump never lands on
# disk in cleartext.
pg_dump \
    --format=custom \
    --no-owner \
    --no-privileges \
    "$DATABASE_URL" \
  | age -R "$recipients" \
  | aws s3 cp \
      "${aws_extra[@]}" \
      --no-progress \
      --expected-size 0 \
      - \
      "$target"

log "backup complete -> $target"
