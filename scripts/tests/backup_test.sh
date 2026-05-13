#!/usr/bin/env bash
# Integration tests for scripts/backup.sh (SIN-62267).
#
# We do not have bats in the deps allowlist, so this is a hand-rolled bash
# test runner. Each test spins up an isolated $TEST_DIR with stub binaries
# on PATH for pg_dump, age, aws, logger, and hostname. The stubs read their
# behavior from env vars exported by the test, so we can drive every silent-
# failure scenario the issue calls out.
#
# Run with `bash scripts/tests/backup_test.sh`. Exit 0 on success, 1 on any
# failed assertion. Output is verbose enough to debug a failing assertion
# without re-running with -x.
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null && pwd)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/../.." >/dev/null && pwd)
BACKUP_SCRIPT="$REPO_ROOT/scripts/backup.sh"

[[ -x "$BACKUP_SCRIPT" ]] || { echo "FATAL: $BACKUP_SCRIPT not executable"; exit 1; }

PASS=0
FAIL=0
FAILED_NAMES=()

# -----------------------------------------------------------------------------
# Test helpers
# -----------------------------------------------------------------------------

# stage_env builds a fresh per-test sandbox under $TEST_DIR with stub
# binaries that the test can parameterize via the BACKUP_TEST_* env vars.
# Sourcing this adds $STUB_DIR to PATH (front) and exports paths the test
# can inspect after the script under test runs.
stage_env() {
  TEST_DIR=$(mktemp -d -t sin62267-backup-test.XXXXXX)
  STUB_DIR="$TEST_DIR/bin"
  STATE_DIR="$TEST_DIR/state"
  TMP_STAGE_DIR="$TEST_DIR/tmp"
  LOG_FILE="$TEST_DIR/journald.log"
  S3_DIR="$TEST_DIR/s3"
  RECIPIENTS="$TEST_DIR/age-backup.pub"
  mkdir -p "$STUB_DIR" "$STATE_DIR" "$TMP_STAGE_DIR" "$S3_DIR"

  # The stub age binary checks recipients exist+readable. Content does not
  # matter — backup.sh just passes the path to age -R.
  cat >"$RECIPIENTS" <<'EOF'
# fake age recipient for tests; real one lives in infra/age-backup.pub
age1placeholder000000000000000000000000000000000000000000000000
EOF

  # ----- pg_dump stub ------------------------------------------------------
  cat >"$STUB_DIR/pg_dump" <<'STUB'
#!/usr/bin/env bash
# Test stub for pg_dump. Writes BACKUP_TEST_PG_DUMP_BYTES bytes to stdout
# and exits with BACKUP_TEST_PG_DUMP_EXIT.
bytes=${BACKUP_TEST_PG_DUMP_BYTES:-0}
if (( bytes > 0 )); then
  head -c "$bytes" /dev/zero
fi
exit "${BACKUP_TEST_PG_DUMP_EXIT:-0}"
STUB
  chmod +x "$STUB_DIR/pg_dump"

  # ----- age stub ----------------------------------------------------------
  cat >"$STUB_DIR/age" <<'STUB'
#!/usr/bin/env bash
# Test stub for age. Two modes:
#   age --version          -> echo BACKUP_TEST_AGE_VERSION (default v1.1.1)
#   age -R RCP -o OUT IN   -> copy IN -> OUT, exit BACKUP_TEST_AGE_EXIT
if [[ "${1:-}" == "--version" ]]; then
  echo "${BACKUP_TEST_AGE_VERSION:-v1.1.1}"
  exit 0
fi
src=""; dst=""
while (( $# )); do
  case "$1" in
    -R) shift; recipients=$1 ;;
    -o) shift; dst=$1 ;;
    -*) ;;
    *)  src=$1 ;;
  esac
  shift || true
done
exit_code=${BACKUP_TEST_AGE_EXIT:-0}
if (( exit_code == 0 )); then
  if [[ -n "$src" && -n "$dst" ]]; then
    # Pretend to encrypt: prepend a fake header + copy.
    {
      printf 'AGE-FAKE-HEADER\n'
      cat "$src"
    } > "$dst"
  fi
fi
exit "$exit_code"
STUB
  chmod +x "$STUB_DIR/age"

  # ----- aws stub ----------------------------------------------------------
  # Dispatches on `aws <service> <op>`:
  #   aws s3 cp [opts] SRC DEST   -> copy SRC into $S3_DIR/<bucket>/<key>
  #   aws s3api head-object       -> echo content length, optionally fail
  cat >"$STUB_DIR/aws" <<STUB
#!/usr/bin/env bash
S3_DIR=$S3_DIR
STUB
  cat >>"$STUB_DIR/aws" <<'STUB'
service=${1:-}; shift || true
op=${1:-}; shift || true

# Strip optional global --endpoint-url <url> in any position.
args=()
while (( $# )); do
  case "$1" in
    --endpoint-url) shift; shift; continue ;;
    *) args+=("$1"); shift ;;
  esac
done
set -- "${args[@]}"

case "$service/$op" in
  s3/cp)
    # Find SRC and DEST: last two positional args after stripping options.
    pos=()
    while (( $# )); do
      case "$1" in
        --no-progress) shift ;;
        --expected-size) shift; shift ;;
        --*) shift ;;
        *) pos+=("$1"); shift ;;
      esac
    done
    src=${pos[0]:-}
    dest=${pos[1]:-}
    rc=${BACKUP_TEST_AWS_CP_EXIT:-0}
    if (( rc == 0 )); then
      # dest looks like s3://bucket/key/path
      if [[ "$dest" =~ ^s3://([^/]+)/(.+)$ ]]; then
        bucket=${BASH_REMATCH[1]}
        key=${BASH_REMATCH[2]}
        mkdir -p "$S3_DIR/$bucket/$(dirname "$key")"
        if [[ -n "${BACKUP_TEST_AWS_CP_DROP:-}" ]]; then
          # Simulate the silent-drop case: cp returns 0 but no object lands.
          :
        else
          cp -- "$src" "$S3_DIR/$bucket/$key"
        fi
      fi
    fi
    exit "$rc"
    ;;
  s3api/head-object)
    bucket=""; key=""; query=""; output=""
    while (( $# )); do
      case "$1" in
        --bucket) shift; bucket=$1 ;;
        --key) shift; key=$1 ;;
        --query) shift; query=$1 ;;
        --output) shift; output=$1 ;;
      esac
      shift || true
    done
    rc=${BACKUP_TEST_AWS_HEAD_EXIT:-0}
    if (( rc != 0 )); then
      echo "stub: head-object forced failure" >&2
      exit "$rc"
    fi
    obj="$S3_DIR/$bucket/$key"
    if [[ ! -f "$obj" ]]; then
      echo "stub: object missing $obj" >&2
      exit 254
    fi
    actual=$(stat -c %s -- "$obj")
    if [[ -n "${BACKUP_TEST_AWS_HEAD_OVERRIDE_BYTES:-}" ]]; then
      actual=$BACKUP_TEST_AWS_HEAD_OVERRIDE_BYTES
    fi
    if [[ "$query" == "ContentLength" && "$output" == "text" ]]; then
      printf '%s\n' "$actual"
      exit 0
    fi
    printf '{"ContentLength": %s}\n' "$actual"
    exit 0
    ;;
  *)
    echo "stub: unsupported aws subcommand: $service $op" >&2
    exit 2
    ;;
esac
STUB
  chmod +x "$STUB_DIR/aws"

  # ----- logger stub -------------------------------------------------------
  # backup.sh calls `logger -t TAG -p user.LEVEL -- MESSAGE`. We capture the
  # invocation as `LEVEL\tMESSAGE` per line for easy assertion.
  cat >"$STUB_DIR/logger" <<STUB
#!/usr/bin/env bash
LOG_FILE=$LOG_FILE
STUB
  cat >>"$STUB_DIR/logger" <<'STUB'
tag=""; level=""; msg=""
while (( $# )); do
  case "$1" in
    -t) shift; tag=$1 ;;
    -p) shift; level=$1 ;;
    --) shift; msg="$*"; break ;;
    *) ;;
  esac
  shift || true
done
printf '%s\t%s\t%s\n' "$tag" "$level" "$msg" >>"$LOG_FILE"
STUB
  chmod +x "$STUB_DIR/logger"

  # ----- hostname stub -----------------------------------------------------
  cat >"$STUB_DIR/hostname" <<'STUB'
#!/usr/bin/env bash
echo "${BACKUP_TEST_HOSTNAME:-test-node}"
STUB
  chmod +x "$STUB_DIR/hostname"

  PATH="$STUB_DIR:$PATH"
  export PATH
}

teardown_env() {
  if [[ -n "${TEST_DIR:-}" && -d "$TEST_DIR" && "${KEEP_TEST_DIR:-0}" != "1" ]]; then
    rm -rf -- "$TEST_DIR"
  fi
}

# run_backup invokes backup.sh against the staged sandbox and captures the
# exit code into $RC. Stdout/stderr from the script are tee'd to the test
# log so a failed assertion can show what the script actually did.
run_backup() {
  set +e
  (
    export DATABASE_URL=${DATABASE_URL:-postgres://stub/local}
    export BACKUP_BUCKET=${BACKUP_BUCKET:-test-bucket}
    export BACKUP_AGE_RECIPIENTS=$RECIPIENTS
    export BACKUP_NODE_ID=${BACKUP_NODE_ID:-test-node}
    export BACKUP_STATE_DIR=$STATE_DIR
    export BACKUP_TMPDIR=$TMP_STAGE_DIR
    bash "$BACKUP_SCRIPT"
  ) >"$TEST_DIR/script.stdout" 2>"$TEST_DIR/script.stderr"
  RC=$?
  set -e
}

# log_has greps the captured logger output for a substring.
log_has() {
  grep -qF -- "$1" "$LOG_FILE"
}

# log_has_level looks for a substring at the given syslog level. The logger
# stub records `-p user.LEVEL` verbatim (e.g. "user.err"), so we match the
# trailing facility component.
log_has_level() {
  local level=$1; shift
  awk -v lvl="$level" -v needle="$1" -F'\t' '
    {
      sub(/^.*\./, "", $2)
      if ($2 == lvl && index($3, needle)) found = 1
    }
    END { exit !found }
  ' "$LOG_FILE"
}

assert_eq() {
  local got=$1 want=$2 msg=$3
  if [[ "$got" != "$want" ]]; then
    echo "  assert_eq FAIL [$msg]: got=$got want=$want"
    return 1
  fi
}

assert_true() {
  local cond=$1 msg=$2
  if ! eval "$cond"; then
    echo "  assert_true FAIL [$msg]: cond '$cond' was false"
    return 1
  fi
}

assert_false() {
  local cond=$1 msg=$2
  if eval "$cond"; then
    echo "  assert_false FAIL [$msg]: cond '$cond' was true"
    return 1
  fi
}

# run_test wraps each test case so we can collect pass/fail without
# letting one assertion abort the whole runner.
run_test() {
  local name=$1
  local fn=$2
  echo "── $name"
  local errors=0
  (
    set +e
    "$fn"
  )
  local rc=$?
  if (( rc == 0 )); then
    echo "  ok"
    PASS=$((PASS + 1))
  else
    echo "  FAIL ($name)"
    FAIL=$((FAIL + 1))
    FAILED_NAMES+=("$name")
  fi
}

# Each test_xxx function:
#   1. unsets BACKUP_TEST_* env from prior test
#   2. sets stub behavior via BACKUP_TEST_*
#   3. calls stage_env, run_backup, then asserts
#   4. calls teardown_env on exit (trap)
clear_test_env() {
  unset BACKUP_TEST_PG_DUMP_BYTES BACKUP_TEST_PG_DUMP_EXIT
  unset BACKUP_TEST_AGE_EXIT BACKUP_TEST_AGE_VERSION
  unset BACKUP_TEST_AWS_CP_EXIT BACKUP_TEST_AWS_CP_DROP
  unset BACKUP_TEST_AWS_HEAD_EXIT BACKUP_TEST_AWS_HEAD_OVERRIDE_BYTES
  unset BACKUP_TEST_HOSTNAME
}

# -----------------------------------------------------------------------------
# Test cases
# -----------------------------------------------------------------------------

test_golden_bootstrap() {
  clear_test_env
  stage_env
  trap teardown_env RETURN
  export BACKUP_TEST_PG_DUMP_BYTES=$((2 * 1024 * 1024))   # 2 MiB
  run_backup
  assert_eq "$RC" 0 "exit code"                                       || return 1
  assert_true "[[ -f \"$STATE_DIR/backup-last-success.json\" ]]" "state file written" || return 1
  assert_true "log_has_level info 'stage=done status=ok'" "done log line"             || return 1
  assert_true "log_has 'bootstrap=true'" "bootstrap=true logged on first run"         || return 1
  # ciphertext landed in fake S3
  assert_true "compgen -G \"$S3_DIR/test-bucket/*/test-node/dump.pgc.age\" >/dev/null" "object uploaded" || return 1
  # state file contains the dump bytes we generated
  assert_true "grep -q '\"dump_bytes\":2097152' \"$STATE_DIR/backup-last-success.json\"" "state dump_bytes recorded" || return 1
}

test_pg_dump_silent_truncation() {
  # pg_dump exits 0 but only writes a few bytes (e.g. seccomp killed it
  # mid-dump). Threshold check must catch this.
  clear_test_env
  stage_env
  trap teardown_env RETURN
  export BACKUP_TEST_PG_DUMP_BYTES=4096   # 4 KiB, below 1 MiB floor
  export BACKUP_TEST_PG_DUMP_EXIT=0
  run_backup
  assert_true "(( RC != 0 ))" "exit non-zero"                                   || return 1
  assert_true "log_has_level err 'stage=pg_dump status=fail'" "pg_dump fail log" || return 1
  assert_true "log_has 'size-below-threshold'" "size-below-threshold reason"     || return 1
  assert_false "[[ -f \"$STATE_DIR/backup-last-success.json\" ]]" "state file NOT written on failure" || return 1
}

test_pg_dump_exit_nonzero() {
  clear_test_env
  stage_env
  trap teardown_env RETURN
  export BACKUP_TEST_PG_DUMP_BYTES=$((2 * 1024 * 1024))
  export BACKUP_TEST_PG_DUMP_EXIT=42
  run_backup
  assert_true "(( RC != 0 ))" "exit non-zero"                                  || return 1
  assert_true "log_has_level err 'stage=pg_dump status=fail'" "pg_dump fail log" || return 1
  assert_false "[[ -f \"$STATE_DIR/backup-last-success.json\" ]]" "no state on failure" || return 1
}

test_encrypt_failure() {
  clear_test_env
  stage_env
  trap teardown_env RETURN
  export BACKUP_TEST_PG_DUMP_BYTES=$((2 * 1024 * 1024))
  export BACKUP_TEST_AGE_EXIT=7
  run_backup
  assert_true "(( RC != 0 ))" "exit non-zero"                                   || return 1
  assert_true "log_has_level err 'stage=encrypt status=fail'" "encrypt fail log" || return 1
  assert_false "[[ -f \"$STATE_DIR/backup-last-success.json\" ]]" "no state on failure" || return 1
}

test_upload_failure() {
  clear_test_env
  stage_env
  trap teardown_env RETURN
  export BACKUP_TEST_PG_DUMP_BYTES=$((2 * 1024 * 1024))
  export BACKUP_TEST_AWS_CP_EXIT=255
  run_backup
  assert_true "(( RC != 0 ))" "exit non-zero"                                  || return 1
  assert_true "log_has_level err 'stage=upload status=fail'" "upload fail log"   || return 1
  assert_false "[[ -f \"$STATE_DIR/backup-last-success.json\" ]]" "no state on failure" || return 1
}

test_head_object_missing() {
  # aws s3 cp returns 0 but the object never lands (silent drop). head-object
  # must catch this with a non-zero exit and the script must fail.
  clear_test_env
  stage_env
  trap teardown_env RETURN
  export BACKUP_TEST_PG_DUMP_BYTES=$((2 * 1024 * 1024))
  export BACKUP_TEST_AWS_CP_DROP=1
  run_backup
  assert_true "(( RC != 0 ))" "exit non-zero"                                  || return 1
  assert_true "log_has_level err 'stage=verify status=fail'" "verify fail log"   || return 1
  assert_true "log_has 'head-object-failed'" "head-object reason"               || return 1
  assert_false "[[ -f \"$STATE_DIR/backup-last-success.json\" ]]" "no state on failure" || return 1
}

test_head_object_size_mismatch() {
  clear_test_env
  stage_env
  trap teardown_env RETURN
  export BACKUP_TEST_PG_DUMP_BYTES=$((2 * 1024 * 1024))
  export BACKUP_TEST_AWS_HEAD_OVERRIDE_BYTES=1   # remote claims 1 byte
  run_backup
  assert_true "(( RC != 0 ))" "exit non-zero"                                  || return 1
  assert_true "log_has_level err 'stage=verify status=fail'" "verify fail log"   || return 1
  assert_true "log_has 'content-length-mismatch'" "mismatch reason"             || return 1
  assert_false "[[ -f \"$STATE_DIR/backup-last-success.json\" ]]" "no state on failure" || return 1
}

test_threshold_from_state_file() {
  # State file says last successful dump was 100 MiB. New run produces a
  # 5 MiB dump (5%, below the 10% floor). Must fail at threshold check.
  clear_test_env
  stage_env
  trap teardown_env RETURN
  cat >"$STATE_DIR/backup-last-success.json" <<EOF
{"dump_bytes":104857600,"enc_bytes":104857700,"object":"prev/key","node_id":"test-node","timestamp":"2026-04-01T00:00:00Z"}
EOF
  export BACKUP_TEST_PG_DUMP_BYTES=$((5 * 1024 * 1024))   # 5 MiB
  run_backup
  assert_true "(( RC != 0 ))" "exit non-zero"                                  || return 1
  assert_true "log_has_level err 'stage=pg_dump status=fail'" "pg_dump fail log" || return 1
  assert_true "log_has 'size-below-threshold'" "size-below-threshold reason"     || return 1
  # State file MUST be preserved (we read 104857600 from it earlier; it must
  # still say so afterwards).
  assert_true "grep -q '\"dump_bytes\":104857600' \"$STATE_DIR/backup-last-success.json\"" "state preserved on failure" || return 1
  assert_true "log_has 'min=10485760'" "min computed as 10% of 100 MiB"           || return 1
}

test_threshold_from_state_file_passes() {
  # Dump comes in at 50 MiB which is above the 10% floor of last 100 MiB.
  clear_test_env
  stage_env
  trap teardown_env RETURN
  cat >"$STATE_DIR/backup-last-success.json" <<EOF
{"dump_bytes":104857600,"enc_bytes":104857700,"object":"prev/key","node_id":"test-node","timestamp":"2026-04-01T00:00:00Z"}
EOF
  export BACKUP_TEST_PG_DUMP_BYTES=$((50 * 1024 * 1024))
  run_backup
  assert_eq "$RC" 0 "exit code"                                                  || return 1
  assert_true "log_has 'bootstrap=false'" "not bootstrap when state present"      || return 1
  assert_true "grep -q '\"dump_bytes\":52428800' \"$STATE_DIR/backup-last-success.json\"" "state updated to new bytes" || return 1
}

test_age_too_old_blocks_run() {
  clear_test_env
  stage_env
  trap teardown_env RETURN
  export BACKUP_TEST_AGE_VERSION="0.10.0"
  export BACKUP_TEST_PG_DUMP_BYTES=$((2 * 1024 * 1024))
  run_backup
  assert_true "(( RC != 0 ))" "exit non-zero"                                   || return 1
  assert_true "log_has_level err 'stage=preflight status=fail'" "preflight fail log" || return 1
  assert_false "[[ -f \"$STATE_DIR/backup-last-success.json\" ]]" "no state on preflight failure" || return 1
}

test_no_dump_bytes_in_logs() {
  # Sanity: the structured logs must NOT include any byte-content from the
  # dump. We seed pg_dump with a recognizable byte pattern and assert the
  # marker never makes it into the journald log file.
  clear_test_env
  stage_env
  trap teardown_env RETURN
  cat >"$STUB_DIR/pg_dump" <<'STUB'
#!/usr/bin/env bash
# Emit 2 MiB starting with a recognizable marker.
printf 'SECRETMARKER-DO-NOT-LEAK '
head -c $((2 * 1024 * 1024 - 24)) /dev/zero
exit 0
STUB
  chmod +x "$STUB_DIR/pg_dump"
  run_backup
  assert_eq "$RC" 0 "exit code"                                                  || return 1
  assert_false "log_has 'SECRETMARKER'" "dump bytes never reach journald log"     || return 1
}

test_database_url_not_logged() {
  # DATABASE_URL contains a password — must never appear in the log file.
  clear_test_env
  stage_env
  trap teardown_env RETURN
  export DATABASE_URL="postgres://user:s3cret-p4ss@db.example/sin?sslmode=require"
  export BACKUP_TEST_PG_DUMP_BYTES=$((2 * 1024 * 1024))
  run_backup
  assert_eq "$RC" 0 "exit code"                                                  || return 1
  assert_false "log_has 's3cret-p4ss'" "password from URL not logged"              || return 1
  assert_false "log_has 'postgres://'" "DATABASE_URL prefix not logged"            || return 1
}

# -----------------------------------------------------------------------------
# Driver
# -----------------------------------------------------------------------------

main() {
  echo "Running scripts/backup.sh tests"
  echo "  script:    $BACKUP_SCRIPT"
  echo "  repo_root: $REPO_ROOT"
  echo

  run_test "golden bootstrap (no state file)"             test_golden_bootstrap
  run_test "pg_dump silent truncation below 1 MiB floor"  test_pg_dump_silent_truncation
  run_test "pg_dump exits non-zero"                       test_pg_dump_exit_nonzero
  run_test "age encrypt fails"                            test_encrypt_failure
  run_test "aws s3 cp fails"                              test_upload_failure
  run_test "head-object missing object"                   test_head_object_missing
  run_test "head-object size mismatch"                    test_head_object_size_mismatch
  run_test "threshold from state — below 10%"             test_threshold_from_state_file
  run_test "threshold from state — above 10%"             test_threshold_from_state_file_passes
  run_test "age <1.0 fails preflight"                     test_age_too_old_blocks_run
  run_test "dump bytes never logged"                      test_no_dump_bytes_in_logs
  run_test "DATABASE_URL never logged"                    test_database_url_not_logged

  echo
  echo "RESULT: $PASS passed, $FAIL failed"
  if (( FAIL > 0 )); then
    printf '  FAILED: %s\n' "${FAILED_NAMES[@]}"
    exit 1
  fi
}

main "$@"
