#!/usr/bin/env bash
# Encrypted Postgres backup for Sindireceita.
#
# Pipeline: pg_dump (custom format) -> age (X25519 recipient) -> aws s3 cp.
# The dump is encrypted client-side BEFORE it ever touches the bucket so a
# leaked AWS/B2 credential cannot decrypt the dumps.
#
# Defensive layout (SIN-62267): each stage runs as its own command with an
# explicit exit-code check. The dump and the ciphertext land on tmpfs (the
# sidecar container ships with `tmpfs: [/tmp]` + `read_only: true` — see
# deploy/compose/compose.stg.yml service `backup` and ADR 0102) so we can
# size-check the cleartext dump, size-check the ciphertext upload, and
# head-object-verify the remote object. Any silent kill (e.g. seccomp
# filtering pg_dump in the hardened container) flips at least one of those
# checks and the script exits non-zero with a structured log line.
#
# Required env:
#   DATABASE_URL    libpq URL of the source DB.
#   BACKUP_BUCKET   S3 bucket name (no scheme, no trailing slash).
# Optional env:
#   BACKUP_AGE_RECIPIENTS  recipients file (default: infra/age-backup.pub
#                          baked into the sidecar image at
#                          /opt/sindireceita/infra/age-backup.pub).
#   BACKUP_PREFIX          object key prefix (default: empty -> dated dir).
#   BACKUP_NODE_ID         per-host segregation (default: hostname -s).
#   BACKUP_STATE_DIR       state directory (default: /var/lib/sindireceita).
#   BACKUP_TMPDIR          where to stage the dump + ciphertext
#                          (default: $TMPDIR or /tmp).
#   AWS_ENDPOINT_URL       custom S3 endpoint (e.g. Backblaze B2).
#
# Logs are emitted as key=value tokens on stderr, ISO-8601 timestamp first.
# Docker captures stderr and promtail/Loki parses the key=value pairs (same
# path every other service in compose.stg.yml uses). The legacy host-systemd
# regime piped through the syslog tag interface (see ADR 0102 § "Hardening
# invariant mapping" for the journald row); the sidecar regime drops that
# hop because we no longer have a journald sink on the container side.
# See ADR 0102 § "Consequences" → "We lose journald-native structured
# fields" for the trade.
#
# DO NOT echo secrets, dump bytes, or DATABASE_URL — the container log
# stream rides the same Loki ingest as the rest of the stack, which is
# accessible to anyone with platform-engineer Grafana access.
#
# SIN-62250 introduced the encrypt-then-upload pipeline; SIN-62267 added the
# explicit per-stage exit-code chain, the size threshold, the head-object
# verify, and the structured logs; SIN-63195 ported the script from the
# legacy host-systemd unit to the compose sidecar regime.
set -Eeuo pipefail
set -o errtrace
shopt -s inherit_errexit

readonly LOG_TAG="sindireceita-backup"
readonly MIN_BYTES_FLOOR=$((1 * 1024 * 1024))      # 1 MiB
readonly LAST_SUCCESS_FLOOR_FRACTION_PCT=10        # 10% of last success

# sblog emits a single structured key=value line on stderr at the given
# severity. Docker captures stderr verbatim and promtail/Loki parses the
# tokens (ts=…, level=…, service=…, plus whatever the caller passed).
# Stderr (not stdout) is deliberate: stdout is reserved for any future
# pipe redirection (e.g. if a stage ever streams content) so structured
# logs never get interleaved with payload bytes.
sblog() {
  local level=$1
  shift
  printf 'ts=%s level=%s service=%s %s\n' "$(date -u +%FT%TZ)" "$level" "$LOG_TAG" "$*" >&2
}

now_ms() { date +%s%3N; }

# fail logs a structured failure line at err level and exits with the given
# code. The state file is intentionally NOT touched so the threshold derived
# from the last successful run keeps protecting the next attempt.
fail() {
  local stage=$1
  local reason=$2
  local code=${3:-1}
  sblog err "stage=${stage} status=fail code=${code} reason=${reason}"
  exit "$code"
}

cleanup() {
  if [[ -n "${tmp_dump:-}" && -e "${tmp_dump}" ]]; then rm -f -- "$tmp_dump"; fi
  if [[ -n "${tmp_enc:-}" && -e "${tmp_enc}" ]]; then rm -f -- "$tmp_enc"; fi
}
trap cleanup EXIT
# Catch-all for command failures that bypass the explicit `if ! ...; then`
# checks. We still want a structured log line in that path.
trap 'fail unknown "errtrace_line=${LINENO}" 1' ERR

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
    ''|*[!0-9]*) fail preflight "age-version-unparseable raw=${raw:-empty} (age >= 1.0 required)" 1 ;;
  esac
  if (( major < 1 )); then
    # NOTE: phrase "age >= 1.0 required" is asserted by
    # internal/backup.TestBackupScriptRejectsOldAge — keep it verbatim.
    fail preflight "age >= 1.0 required (got: ${raw}); v0.x lacks HMAC tamper protection" 1
  fi
}

# read_last_dump_bytes pulls the last successful dump_bytes value from the
# JSON state file. It avoids jq (not in the deps allowlist) by using a
# simple regex match on the well-known shape we write below.
read_last_dump_bytes() {
  local file=$1
  [[ -f "$file" ]] || { printf '0'; return 0; }
  local line
  line=$(grep -oE '"dump_bytes"[[:space:]]*:[[:space:]]*[0-9]+' "$file" 2>/dev/null | head -1 || true)
  if [[ -z "$line" ]]; then
    printf '0'
    return 0
  fi
  printf '%s' "${line##*:}" | tr -d '[:space:]'
}

: "${DATABASE_URL:?DATABASE_URL must be set}"
: "${BACKUP_BUCKET:?BACKUP_BUCKET must be set}"

require_age_v1

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null && pwd)
repo_root=$(cd -- "$script_dir/.." >/dev/null && pwd)
recipients=${BACKUP_AGE_RECIPIENTS:-"$repo_root/infra/age-backup.pub"}
[[ -r "$recipients" ]] || fail preflight "recipients-not-readable path=${recipients}" 1

prefix=${BACKUP_PREFIX:-}
date_dir=$(date -u +%F)
node_id=${BACKUP_NODE_ID:-$(hostname -s)}
[[ -n "$node_id" ]] || fail preflight "no-node-id" 1
object="${prefix:+$prefix/}$date_dir/$node_id/dump.pgc.age"
target="s3://${BACKUP_BUCKET}/${object}"

state_dir=${BACKUP_STATE_DIR:-/var/lib/sindireceita}
state_file="$state_dir/backup-last-success.json"
last_bytes=$(read_last_dump_bytes "$state_file")
[[ "$last_bytes" =~ ^[0-9]+$ ]] || last_bytes=0

bootstrap=false
if (( last_bytes <= 0 )); then
  bootstrap=true
fi
threshold_from_last=$(( last_bytes * LAST_SUCCESS_FLOOR_FRACTION_PCT / 100 ))
min_size=$MIN_BYTES_FLOOR
if (( threshold_from_last > min_size )); then
  min_size=$threshold_from_last
fi

aws_extra=()
if [[ -n "${AWS_ENDPOINT_URL:-}" ]]; then
  aws_extra+=(--endpoint-url "$AWS_ENDPOINT_URL")
fi

tmp_root=${BACKUP_TMPDIR:-${TMPDIR:-/tmp}}
tmp_dump=$(mktemp "$tmp_root/sindireceita-dump.XXXXXX.pgc")
tmp_enc="${tmp_dump}.age"

sblog info "stage=start bootstrap=${bootstrap} min_bytes=${min_size} last_bytes=${last_bytes} target=${target}"

# Stage 1: pg_dump. Land cleartext on tmpfs so we can size-check it before
# spending CPU on encryption + bandwidth on upload. The sidecar's `tmpfs:
# [/tmp]` mount guarantees this never persists across runs.
t0=$(now_ms)
if ! pg_dump --format=custom --no-owner --no-privileges "$DATABASE_URL" >"$tmp_dump"; then
  fail pg_dump "exit=$?" 1
fi
dur_pg_dump=$(( $(now_ms) - t0 ))
dump_bytes=$(stat -c %s -- "$tmp_dump")
sblog info "stage=pg_dump status=ok dur_ms=${dur_pg_dump} bytes=${dump_bytes}"

if (( dump_bytes < min_size )); then
  fail pg_dump "size-below-threshold bytes=${dump_bytes} min=${min_size} bootstrap=${bootstrap}" 1
fi

# Stage 2: encrypt to a sibling tmpfile. age normally streams stdin->stdout;
# using -o lets us check exit code AND ciphertext size in two boring steps.
t0=$(now_ms)
if ! age -R "$recipients" -o "$tmp_enc" "$tmp_dump"; then
  fail encrypt "exit=$?" 1
fi
dur_encrypt=$(( $(now_ms) - t0 ))
enc_bytes=$(stat -c %s -- "$tmp_enc")
if (( enc_bytes <= 0 )); then
  fail encrypt "ciphertext-empty bytes=${enc_bytes}" 1
fi
sblog info "stage=encrypt status=ok dur_ms=${dur_encrypt} bytes=${enc_bytes}"

# Stage 3: upload the ciphertext file. Passing the path (not a stream) makes
# aws s3 cp respect --expected-size accurately and lets us run head-object
# right after with a known content length to compare against.
t0=$(now_ms)
if ! aws s3 cp \
        "${aws_extra[@]}" \
        --no-progress \
        --expected-size "$enc_bytes" \
        "$tmp_enc" \
        "$target"; then
  fail upload "exit=$?" 1
fi
dur_upload=$(( $(now_ms) - t0 ))
sblog info "stage=upload status=ok dur_ms=${dur_upload} bytes=${enc_bytes}"

# Stage 4: head-object verify. aws s3 cp can return 0 even if the final put
# silently dropped (rare but documented around S3 503 retry exhaustion). We
# trust head-object as the authoritative "the bucket has it" signal.
t0=$(now_ms)
remote_bytes=$(aws s3api head-object \
                  "${aws_extra[@]}" \
                  --bucket "$BACKUP_BUCKET" \
                  --key "$object" \
                  --query 'ContentLength' \
                  --output text 2>/dev/null) \
  || fail verify "head-object-failed exit=$? key=${object}" 1
dur_verify=$(( $(now_ms) - t0 ))

[[ "$remote_bytes" =~ ^[0-9]+$ ]] \
  || fail verify "content-length-unparseable raw=${remote_bytes}" 1
if (( remote_bytes != enc_bytes )); then
  fail verify "content-length-mismatch local=${enc_bytes} remote=${remote_bytes}" 1
fi
sblog info "stage=verify status=ok dur_ms=${dur_verify} bytes=${remote_bytes}"

# Stage 5: persist last-success metadata. Atomic rename keeps the file
# either fully old or fully new — never half-written if we get interrupted.
# The state file is only written when EVERY prior stage succeeded.
if ! mkdir -p -- "$state_dir"; then
  fail state "mkdir-state-dir-failed path=${state_dir}" 1
fi
state_tmp=$(mktemp "$state_dir/.backup-last-success.XXXXXX")
printf '{"dump_bytes":%d,"enc_bytes":%d,"object":"%s","node_id":"%s","timestamp":"%s"}\n' \
  "$dump_bytes" "$enc_bytes" "$object" "$node_id" "$(date -u +%FT%TZ)" \
  >"$state_tmp"
if ! mv -f -- "$state_tmp" "$state_file"; then
  rm -f -- "$state_tmp" || true
  fail state "rename-state-file-failed path=${state_file}" 1
fi

sblog info "stage=done status=ok bytes=${enc_bytes} dump_bytes=${dump_bytes} target=${target}"
